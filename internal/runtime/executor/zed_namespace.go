package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const zedNamespaceAliasPrefix = "zedns_"

type zedToolName struct{ Namespace, Name string }
type zedToolNames map[string]zedToolName

// zedNamespaceAlias derives the stable request-local name for a namespaced tool.
func zedNamespaceAlias(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	return zedNamespaceAliasPrefix + hex.EncodeToString(sum[:16])
}

// Zed accepts native function/custom tools but not the namespace wrapper.
// Use request-local reversible names, including when replaying native tool
// history and forcing a namespaced tool through tool_choice.
func zedFlattenNamespaces(payload []byte) ([]byte, zedToolNames, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, nil, err
	}
	names := zedToolNames{}
	changed := false
	var tools []map[string]json.RawMessage
	if json.Unmarshal(request["tools"], &tools) == nil {
		flat := make([]map[string]json.RawMessage, 0, len(tools))
		sawNamespace := false
		for _, tool := range tools {
			var kind string
			_ = json.Unmarshal(tool["type"], &kind)
			if kind != "namespace" {
				flat = append(flat, tool)
				continue
			}
			sawNamespace = true
			var namespace string
			if err := json.Unmarshal(tool["name"], &namespace); err != nil || namespace == "" {
				return nil, nil, fmt.Errorf("zed: invalid tool namespace")
			}
			var children []map[string]json.RawMessage
			if err := json.Unmarshal(tool["tools"], &children); err != nil {
				return nil, nil, fmt.Errorf("zed: invalid namespace tools")
			}
			for _, child := range children {
				var name, childKind string
				_ = json.Unmarshal(child["name"], &name)
				_ = json.Unmarshal(child["type"], &childKind)
				if name == "" || (childKind != "function" && childKind != "custom") {
					return nil, nil, fmt.Errorf("zed: unsupported namespaced tool")
				}
				alias := zedNamespaceAlias(namespace, name)
				if _, exists := names[alias]; exists {
					return nil, nil, fmt.Errorf("zed: duplicate namespaced tool")
				}
				names[alias] = zedToolName{namespace, name}
				child["name"], _ = json.Marshal(alias)
				flat = append(flat, child)
			}
		}
		// Rewrite whenever a wrapper was seen, even an empty one: Zed rejects
		// the namespace type itself.
		if sawNamespace {
			for _, tool := range flat {
				var name string
				_ = json.Unmarshal(tool["name"], &name)
				if _, exists := names[name]; exists {
					// Generated aliases must not collide with an unrelated plain tool.
					count := 0
					for _, other := range flat {
						var n string
						_ = json.Unmarshal(other["name"], &n)
						if n == name {
							count++
						}
					}
					if count != 1 {
						return nil, nil, fmt.Errorf("zed: ambiguous tool alias")
					}
				}
			}
			request["tools"], _ = json.Marshal(flat)
			changed = true
		}
	}
	// Aliases are a pure function of (namespace, name), so history items whose
	// namespace is no longer offered still map to the same stable name.
	flatten := func(item map[string]json.RawMessage) bool {
		var name, namespace string
		_ = json.Unmarshal(item["name"], &name)
		if _, has := item["namespace"]; !has {
			return false
		}
		if err := json.Unmarshal(item["namespace"], &namespace); err != nil || namespace == "" || name == "" {
			return false
		}
		alias := zedNamespaceAlias(namespace, name)
		names[alias] = zedToolName{namespace, name}
		item["name"], _ = json.Marshal(alias)
		delete(item, "namespace")
		return true
	}
	var input []map[string]json.RawMessage
	if json.Unmarshal(request["input"], &input) == nil {
		inputChanged := false
		for _, item := range input {
			var kind string
			_ = json.Unmarshal(item["type"], &kind)
			if kind != "function_call" && kind != "custom_tool_call" {
				continue
			}
			if flatten(item) {
				inputChanged = true
			}
		}
		if inputChanged {
			request["input"], _ = json.Marshal(input)
			changed = true
		}
	}
	var choice map[string]json.RawMessage
	if json.Unmarshal(request["tool_choice"], &choice) == nil && choice != nil {
		var kind string
		_ = json.Unmarshal(choice["type"], &kind)
		choiceChanged := (kind == "function" || kind == "custom") && flatten(choice)
		var allowed []map[string]json.RawMessage
		if kind == "allowed_tools" && json.Unmarshal(choice["tools"], &allowed) == nil {
			allowedChanged := false
			for _, entry := range allowed {
				var entryKind string
				_ = json.Unmarshal(entry["type"], &entryKind)
				if (entryKind == "function" || entryKind == "custom") && flatten(entry) {
					allowedChanged = true
				}
			}
			if allowedChanged {
				choice["tools"], _ = json.Marshal(allowed)
				choiceChanged = true
			}
		}
		if choiceChanged {
			request["tool_choice"], _ = json.Marshal(choice)
			changed = true
		}
	}
	if !changed {
		return payload, nil, nil
	}
	out, err := json.Marshal(request)
	return out, names, err
}

// zedRestoreNamespaces maps aliases back to the client's namespaced tools in
// streamed events and terminal responses: output items, argument/input done
// events, and the echoed response.tools and response.tool_choice. Aliases are
// collision-checked hashes, so any object whose name is a known alias is ours.
func zedRestoreNamespaces(data []byte, names zedToolNames) []byte {
	if len(names) == 0 || len(data) == 0 || !bytes.Contains(data, []byte(zedNamespaceAliasPrefix)) {
		return data
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	// Keep numbers verbatim; float64 would corrupt integers above 2^53.
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return data
	}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if name, ok := x["name"].(string); ok {
				if original, found := names[name]; found {
					x["name"] = original.Name
					x["namespace"] = original.Namespace
				}
			}
			for _, child := range x {
				visit(child)
			}
		case []any:
			for _, child := range x {
				visit(child)
			}
		}
	}
	visit(value)
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return data
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}
