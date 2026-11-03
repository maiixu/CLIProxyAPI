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
