package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestZedNamespaceRoundTrip(t *testing.T) {
	input := []byte(`{"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}},{"type":"custom","name":"patch","format":{"type":"text"}}]},{"type":"namespace","name":"other","tools":[{"type":"function","name":"exec"}]},{"type":"function","name":"plain"}],"input":[{"type":"function_call","namespace":"functions","name":"exec","call_id":"c1","arguments":"{\"n\":17}"},{"type":"function_call_output","call_id":"c1","output":"17"}]}`)
	flat, names, err := zedFlattenNamespaces(input)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Tools []map[string]any `json:"tools"`
		Input []map[string]any `json:"input"`
	}
	if err = json.Unmarshal(flat, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Tools) != 4 || len(names) != 3 {
		t.Fatalf("tools=%d names=%d", len(request.Tools), len(names))
	}
	for _, tool := range request.Tools {
		if tool["type"] == "namespace" {
			t.Fatal("namespace wrapper leaked")
		}
	}
	alias := request.Tools[0]["name"].(string)
	if request.Input[0]["name"] != alias || request.Input[0]["namespace"] != nil || request.Input[0]["arguments"] != `{"n":17}` {
		t.Fatal("native history was not reversibly mapped")
	}
	if request.Input[1]["call_id"] != "c1" || request.Input[1]["output"] != "17" {
		t.Fatal("tool result altered")
	}
	for _, event := range []string{
		`{"type":"response.output_item.added","item":{"type":"function_call","name":"` + alias + `","call_id":"c2","arguments":""}}`,
		`{"type":"response.completed","response":{"output":[{"type":"function_call","name":"` + alias + `","call_id":"c2","arguments":"{}"}]}}`,
	} {
		restored := string(zedRestoreNamespaces([]byte(event), names))
		if !strings.Contains(restored, `"namespace":"functions"`) || !strings.Contains(restored, `"name":"exec"`) || strings.Contains(restored, alias) {
			t.Fatal("tool identity not restored", restored)
		}
	}
	if request.Tools[2]["name"] == alias {
		t.Fatal("same name in different namespaces collided")
	}
}
func TestZedNamespacesRejectAmbiguityAndPreservePlainRequests(t *testing.T) {
	plain := []byte(`{"tools":[{"type":"function","name":"exec"}],"input":[]}`)
	out, names, err := zedFlattenNamespaces(plain)
	if err != nil || string(out) != string(plain) || len(names) != 0 {
		t.Fatal("plain request changed")
	}
	for _, bad := range []string{
		`{"tools":[{"type":"namespace","name":"x","tools":[{"type":"namespace","name":"nested"}]}]}`,
		`{"tools":[{"type":"namespace","name":"x","tools":[{"type":"function","name":"same"},{"type":"function","name":"same"}]}]}`,
	} {
		if _, _, err := zedFlattenNamespaces([]byte(bad)); err == nil {
			t.Fatal("ambiguous request accepted")
		}
	}
}

func TestZedNamespaceToolChoiceUsesAlias(t *testing.T) {
	input := []byte(`{"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec"}]}],"tool_choice":{"type":"function","namespace":"functions","name":"exec"},"input":[]}`)
	flat, names, err := zedFlattenNamespaces(input)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice map[string]any   `json:"tool_choice"`
	}
	if err = json.Unmarshal(flat, &request); err != nil {
		t.Fatal(err)
	}
	alias := zedNamespaceAlias("functions", "exec")
	if request.Tools[0]["name"] != alias {
		t.Fatalf("tool alias = %v", request.Tools[0]["name"])
	}
	if request.ToolChoice["name"] != alias || request.ToolChoice["type"] != "function" {
		t.Fatalf("tool_choice not rewritten: %v", request.ToolChoice)
	}
	if _, has := request.ToolChoice["namespace"]; has {
		t.Fatal("tool_choice namespace leaked")
	}
	if names[alias] != (zedToolName{"functions", "exec"}) {
		t.Fatal("alias not reversible")
	}

	// String tool_choice values and plain function choices are left alone.
	for _, keep := range []string{
		`{"tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"}]}],"tool_choice":"auto"}`,
		`{"tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"a"}]},{"type":"function","name":"plain"}],"tool_choice":{"type":"function","name":"plain"}}`,
	} {
		out, _, errKeep := zedFlattenNamespaces([]byte(keep))
		if errKeep != nil {
			t.Fatal(errKeep)
		}
		var original, got map[string]any
		_ = json.Unmarshal([]byte(keep), &original)
		_ = json.Unmarshal(out, &got)
		if !strings.Contains(string(out), `"tool_choice":`) || fmtJSON(original["tool_choice"]) != fmtJSON(got["tool_choice"]) {
			t.Fatalf("tool_choice changed: %s", out)
		}
	}
}

func TestZedNamespaceHistoryWithoutDeclaredNamespace(t *testing.T) {
	for _, input := range []string{
		// Namespace no longer in tools, other namespaced tools remain.
		`{"tools":[{"type":"namespace","name":"current","tools":[{"type":"function","name":"run"}]}],"input":[{"type":"function_call","namespace":"retired","name":"exec","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`,
		// Only plain tools are offered now.
		`{"tools":[{"type":"function","name":"plain"}],"input":[{"type":"function_call","namespace":"retired","name":"exec","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`,
		// No tools at all.
		`{"input":[{"type":"custom_tool_call","namespace":"retired","name":"exec","call_id":"c1","input":"x"},{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}]}`,
	} {
		flat, names, err := zedFlattenNamespaces([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		var request struct {
			Input []map[string]any `json:"input"`
		}
		if err = json.Unmarshal(flat, &request); err != nil {
			t.Fatal(err)
		}
		alias := zedNamespaceAlias("retired", "exec")
		if request.Input[0]["name"] != alias {
			t.Fatalf("history item not aliased: %s", flat)
		}
		if _, has := request.Input[0]["namespace"]; has {
			t.Fatalf("history namespace leaked: %s", flat)
		}
		if request.Input[1]["call_id"] != "c1" || request.Input[1]["output"] != "ok" {
			t.Fatal("tool result altered")
		}
		if names[alias] != (zedToolName{"retired", "exec"}) {
			t.Fatal("history alias not reversible")
		}
		// The alias is a pure function of namespace and name.
		again, _, _ := zedFlattenNamespaces([]byte(input))
		if string(again) != string(flat) {
			t.Fatal("alias derivation is not deterministic")
		}
	}
}

func fmtJSON(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}

func TestZedNamespaceAllowedToolsChoiceUsesAliases(t *testing.T) {
	input := []byte(`{"tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read"}]},{"type":"function","name":"plain"}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"read","namespace":"mcp"},{"type":"function","name":"plain"}]},"input":[]}`)
	flat, names, err := zedFlattenNamespaces(input)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		ToolChoice struct {
			Mode  string           `json:"mode"`
			Tools []map[string]any `json:"tools"`
		} `json:"tool_choice"`
	}
	if err = json.Unmarshal(flat, &request); err != nil {
		t.Fatal(err)
	}
	alias := zedNamespaceAlias("mcp", "read")
	allowed := request.ToolChoice.Tools
	if request.ToolChoice.Mode != "required" || len(allowed) != 2 {
		t.Fatalf("allowed_tools reshaped: %s", flat)
	}
	if allowed[0]["name"] != alias || allowed[0]["namespace"] != nil {
		t.Fatalf("namespaced allowed tool not aliased: %v", allowed[0])
	}
	if allowed[1]["name"] != "plain" {
		t.Fatalf("plain allowed tool changed: %v", allowed[1])
	}
	if names[alias] != (zedToolName{"mcp", "read"}) {
		t.Fatal("alias not reversible")
	}
}

func TestZedEmptyNamespaceIsNotForwarded(t *testing.T) {
	flat, _, err := zedFlattenNamespaces([]byte(`{"tools":[{"type":"namespace","name":"mcp","tools":[]},{"type":"function","name":"plain"}],"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Tools []map[string]any `json:"tools"`
	}
	if err = json.Unmarshal(flat, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Tools) != 1 || request.Tools[0]["name"] != "plain" {
		t.Fatalf("empty namespace wrapper forwarded: %s", flat)
	}
}

func TestZedRestoreNamespacesKeepsNumbersAndEchoedTools(t *testing.T) {
	_, names, err := zedFlattenNamespaces([]byte(`{"tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read"}]}],"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	alias := zedNamespaceAlias("mcp", "read")
	restoredDone := string(zedRestoreNamespaces([]byte(`{"type":"response.function_call_arguments.done","item_id":"fc_1","name":"`+alias+`","arguments":"<a&b>"}`), names))
	if strings.Contains(restoredDone, alias) || !strings.Contains(restoredDone, `"name":"read"`) || !strings.Contains(restoredDone, `"namespace":"mcp"`) {
		t.Fatalf("arguments.done name not restored: %s", restoredDone)
	}
	if !strings.Contains(restoredDone, `"arguments":"<a&b>"`) {
		t.Fatalf("string content re-escaped: %s", restoredDone)
	}

	completed := `{"type":"response.completed","sequence_number":9007199254740993,"response":{"tool_choice":{"type":"function","name":"` + alias + `"},"tools":[{"type":"function","name":"` + alias + `"}],"output":[],"usage":{"input_tokens":9007199254740993,"output_tokens":1.5}}}`
	restored := zedRestoreNamespaces([]byte(completed), names)
	if strings.Contains(string(restored), alias) {
		t.Fatalf("echoed tools/tool_choice not restored: %s", restored)
	}
	var echoed struct {
		Sequence json.Number `json:"sequence_number"`
		Response struct {
			ToolChoice map[string]any   `json:"tool_choice"`
			Tools      []map[string]any `json:"tools"`
			Usage      struct {
				Input  json.Number `json:"input_tokens"`
				Output json.Number `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(restored)))
	decoder.UseNumber()
	if err = decoder.Decode(&echoed); err != nil {
		t.Fatal(err)
	}
	if echoed.Sequence != "9007199254740993" || echoed.Response.Usage.Input != "9007199254740993" || echoed.Response.Usage.Output != "1.5" {
		t.Fatalf("numbers changed: %s", restored)
	}
	for _, tool := range []map[string]any{echoed.Response.ToolChoice, echoed.Response.Tools[0]} {
		if tool["name"] != "read" || tool["namespace"] != "mcp" {
			t.Fatalf("echoed tool not restored: %v", tool)
		}
	}

	plain := []byte(`{"type":"response.output_text.delta","delta":"<x>","n":9007199254740993}`)
	if got := zedRestoreNamespaces(plain, names); string(got) != string(plain) {
		t.Fatalf("event without aliases rewritten: %s", got)
	}
}
