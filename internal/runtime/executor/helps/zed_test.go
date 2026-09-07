package helps

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNewZedErrorDiagnostics(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"plaintext schema error", "failed to parse OpenAI Responses API request: unknown variant namespace", "failed to parse OpenAI Responses API request: unknown variant namespace"},
		{"JSON error string", `{"error":"unsupported tool type namespace"}`, "unsupported tool type namespace"},
		{"JSON string body", `"invalid Responses input"`, "invalid Responses input"},
		{"nested JSON message", `{"error":{"message":"missing field type"}}`, "missing field type"},
		{"whitespace", "  invalid field\r\nname\tvalue  ", "invalid field  name value"},
		{"HTML", "<!DOCTYPE html><html><body>gateway failure</body></html>", "Bad Request"},
		{"embedded HTML", "Proxy failure: <HTML><body>gateway failure</body></HTML>", "Bad Request"},
		{"JSON HTML", `{"error":"<html>gateway failure</html>"}`, "Bad Request"},
		{"empty", " \r\n ", "Bad Request"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := NewZedError(400, []byte(test.body))
			if err.Message != test.want || err.StatusCode() != 400 {
				t.Fatalf("want status400 %q, got status%d %q", test.want, err.StatusCode(), err.Message)
			}
		})
	}
}

func TestNewZedErrorBoundsDiagnosticBodies(t *testing.T) {
	long := strings.Repeat("x", 800) + "PRIVATE_TAIL"
	for _, body := range []string{long, `{"error":"` + long + `"}`, `{"error":{"message":"` + long + `"}}`} {
		err := NewZedError(502, []byte(body))
		if err.Message != strings.Repeat("x", 512)+"..." || strings.Contains(err.Error(), "PRIVATE_TAIL") {
			t.Fatalf("error body was not bounded: length=%d", len(err.Message))
		}
	}
	err := NewZedError(400, []byte(strings.Repeat("x", 511)+"中"+strings.Repeat("x", 10)))
	if !utf8.ValidString(err.Message) || len(err.Message) > 515 {
		t.Fatal("bounded message has invalid UTF-8 or exceeds its bound")
	}
}

func TestNewZedErrorKeepsStructuredClassification(t *testing.T) {
	err := NewZedError(502, []byte(`{"code":429,"message":"quota exhausted","request_id":"req-1","retry_after":2}`))
	if err.Code != 429 || err.RequestID != "req-1" || err.RetryAfter() == nil || err.RetryAfter().Seconds() != 2 {
		t.Fatalf("classification changed: %+v", err)
	}
}
