package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type zedToolName struct{ Namespace, Name string }
type zedToolNames map[string]zedToolName

// Zed accepts native function/custom tools but not the namespace wrapper.
// Use request-local reversible names, including when replaying native tool history.
func zedFlattenNamespaces(payload []byte) ([]byte, zedToolNames, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, nil, err
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(request["tools"], &tools); err != nil {
		return payload, nil, nil
	}
	names := zedToolNames{}
	flat := make([]map[string]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		var kind string
		_ = json.Unmarshal(tool["type"], &kind)
		if kind != "namespace" {
			flat = append(flat, tool)
			continue
		}
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
			sum := sha256.Sum256([]byte(namespace + "\x00" + name))
			alias := "zedns_" + hex.EncodeToString(sum[:16])
			if _, exists := names[alias]; exists {
				return nil, nil, fmt.Errorf("zed: duplicate namespaced tool")
			}
			names[alias] = zedToolName{namespace, name}
			child["name"], _ = json.Marshal(alias)
			flat = append(flat, child)
		}
	}
	if len(names) == 0 {
		return payload, nil, nil
	}
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
	var input []map[string]json.RawMessage
	if json.Unmarshal(request["input"], &input) == nil {
		for _, item := range input {
			var kind, name, namespace string
			_ = json.Unmarshal(item["type"], &kind)
			_ = json.Unmarshal(item["name"], &name)
			_ = json.Unmarshal(item["namespace"], &namespace)
			if kind != "function_call" && kind != "custom_tool_call" {
				continue
			}
			for alias, original := range names {
				if original.Name == name && original.Namespace == namespace {
					item["name"], _ = json.Marshal(alias)
					delete(item, "namespace")
					break
				}
			}
		}
		request["input"], _ = json.Marshal(input)
	}
	out, err := json.Marshal(request)
	return out, names, err
}

// Apply to both streamed item events and terminal response.output arrays.
func zedRestoreNamespaces(data []byte, names zedToolNames) []byte {
	if len(names) == 0 || len(data) == 0 {
		return data
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return data
	}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if kind, _ := x["type"].(string); kind == "function_call" || kind == "custom_tool_call" {
				if name, ok := x["name"].(string); ok {
					if original, found := names[name]; found {
						x["name"] = original.Name
						x["namespace"] = original.Namespace
					}
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
	out, err := json.Marshal(value)
	if err != nil {
		return data
	}
	return out
}
