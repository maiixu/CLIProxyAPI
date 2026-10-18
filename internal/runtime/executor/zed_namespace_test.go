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
