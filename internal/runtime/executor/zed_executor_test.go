package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

var zedTestNow = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

func zedTestJWT(expiry time.Time, marker string) string {
	payload, _ := json.Marshal(map[string]any{"exp": expiry.Unix(), "marker": marker})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".test"
}

func zedTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID: "zed-test.json", Provider: "zed", Prefix: "zed",
		Metadata: map[string]any{
			"type": "zed", "user_id": "17", "credential": map[string]any{"access_token": "test-credential"},
			"access_token": zedTestJWT(zedTestNow.Add(time.Hour), "original"),
		},
	}
}

func zedTestExecutor(t *testing.T, handler http.HandlerFunc) *ZedExecutor {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	executor := NewZedExecutor(&config.Config{})
	executor.baseURL = server.URL
	executor.client = server.Client()
	executor.now = func() time.Time { return zedTestNow }
	return executor
}

func zedTestRequest() (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	return cliproxyexecutor.Request{
		Model:   "zed/gpt-5.6-sol",
		Payload: []byte(`{"model":"zed/gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Read a file."}]}],"max_output_tokens":128,"reasoning":{"effort":"low"}}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
}

const zedTestTerminal = `{"type":"response.completed","response":{"id":"resp_test","model":"gpt-5.6-sol","status":"completed","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}"},{"id":"rs_1","type":"reasoning","encrypted_content":"opaque-reasoning","summary":[]}],"usage":{"input_tokens":21,"output_tokens":9,"total_tokens":30,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":4}}}}`

func TestZedNativeResponsesPreservesToolHistoryAndLimits(t *testing.T) {
	auth := zedTestAuth()
	request, options := zedTestRequest()
	request.Payload = []byte(`{
		"model":"zed/gpt-5.6-sol","stream":false,
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"Follow the task."}]},
			{"type":"reasoning","encrypted_content":"opaque","summary":[]},
			{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"file contents"},
			{"type":"custom_tool_call","call_id":"patch_1","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"patch_1","output":"success"}
		],
		"tools":[
			{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}},"strict":false},
			{"type":"custom","name":"apply_patch","format":{"type":"text"}}
		],
		"reasoning":{"effort":"low","summary":"auto"},"include":["reasoning.encrypted_content"],
		"max_output_tokens":256,"temperature":1,"top_p":1,"store":false,
		"parallel_tool_calls":true,"text":{"format":{"type":"text"}},"metadata":{"task":"test"}
	}`)
	options.Metadata = map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "parent-session"}
	var captured map[string]json.RawMessage
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+auth.Metadata["access_token"].(string) {
			t.Error("wrong inference credential")
		}
		if r.Header.Get("User-Agent") != zedUserAgent {
			t.Error("missing Zed User-Agent")
		}
		if r.Header.Get("x-zed-client-supports-status-messages") != "true" ||
			r.Header.Get("x-zed-client-supports-stream-ended-request-completion-status") != "true" {
			t.Error("missing status negotiation")
		}
		if errDecode := json.NewDecoder(r.Body).Decode(&captured); errDecode != nil {
			t.Error(errDecode)
		}
		w.Header().Set("x-zed-server-supports-status-messages", "true")
		_, _ = fmt.Fprintln(w, `{"status":"started"}`)
		_, _ = fmt.Fprintln(w, `{"status":{"queued":{"position":1}}}`)
		_, _ = fmt.Fprintf(w, "{\"event\":%s}\n", zedTestTerminal)
		_, _ = fmt.Fprintln(w, `{"status":"stream_ended"}`)
	})
	response, errExecute := executor.Execute(context.Background(), auth, request, options)
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	if _, ok := captured["intent"]; ok {
		t.Fatal("obsolete intent field sent")
	}
	if string(captured["provider"]) != `"open_ai"` || string(captured["model"]) != `"gpt-5.6-sol"` {
		t.Fatalf("wrong Zed provider/model: %s / %s", captured["provider"], captured["model"])
	}
	var before, after map[string]any
	_ = json.Unmarshal(request.Payload, &before)
	_ = json.Unmarshal(captured["provider_request"], &after)
	before["model"] = zedSolModel
	before["stream"] = true
	before["input"].([]any)[0].(map[string]any)["role"] = "system"
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Responses payload changed: want %v, got %v", before, after)
	}
	var terminal map[string]json.RawMessage
	_ = json.Unmarshal([]byte(zedTestTerminal), &terminal)
	if !bytes.Equal(response.Payload, terminal["response"]) {
		t.Fatalf("nonstream response not preserved: %s", response.Payload)
	}
	again, errPayload := executor.payload(request, options)
	if errPayload != nil {
		t.Fatal(errPayload)
	}
	var second map[string]json.RawMessage
	_ = json.Unmarshal(again, &second)
	if string(captured["thread_id"]) != string(second["thread_id"]) {
		t.Error("thread ID did not remain stable")
	}
	if string(captured["prompt_id"]) == string(second["prompt_id"]) {
		t.Error("prompt ID was reused")
	}
}

func TestZedStreamingPreservesNativeFunctionAndReasoningEvents(t *testing.T) {
	events := []string{
		`{"type":"response.created","response":{"id":"resp_test"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read_file","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":\"README.md\"}"}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"Reviewing the file."}`,
		zedTestTerminal,
	}
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-zed-server-supports-status-messages", "true")
		_, _ = fmt.Fprintln(w, `{"status":"started"}`)
		for i, event := range events {
			if i%2 == 0 {
				_, _ = fmt.Fprintf(w, "{\"event\":%s}\n", event)
			} else {
				_, _ = fmt.Fprintln(w, event)
			}
		}
		_, _ = fmt.Fprintln(w, `{"status":"stream_ended"}`)
	})
	request, options := zedTestRequest()
	result, errStream := executor.ExecuteStream(context.Background(), zedTestAuth(), request, options)
	if errStream != nil {
		t.Fatal(errStream)
	}
	var got []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		got = append(got, string(chunk.Payload))
	}
	if len(got) != len(events) {
		t.Fatalf("got %d events, want %d", len(got), len(events))
	}
	for i, event := range events {
		if got[i] != "data: "+event+"\n\n" {
			t.Errorf("event %d modified: %s", i, got[i])
		}
	}
}

func TestZedForbiddenDoesNotRefreshCredentials(t *testing.T) {
	var completions, refreshes atomic.Int32
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/client/llm_tokens" {
			refreshes.Add(1)
		}
		if r.URL.Path == "/completions" {
			completions.Add(1)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintln(w, `{"error":{"message":"model is excluded from this plan"}}`)
	})
	request, options := zedTestRequest()
	_, errExecute := executor.Execute(context.Background(), zedTestAuth(), request, options)
	var upstream *helps.ZedError
	if !errors.As(errExecute, &upstream) || upstream.StatusCode() != 403 {
		t.Fatalf("want 403, got %v", errExecute)
	}
	if refreshes.Load() != 0 || completions.Load() != 1 {
		t.Fatalf("403 was retried/refreshed: completions=%d refreshes=%d", completions.Load(), refreshes.Load())
	}
}

func TestZedUnauthorizedRefreshesOnceAndReusesNewToken(t *testing.T) {
	auth := zedTestAuth()
	fresh := zedTestJWT(zedTestNow.Add(2*time.Hour), "refreshed")
	var completions, refreshes atomic.Int32
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client/llm_tokens":
			refreshes.Add(1)
			if r.Header.Get("Authorization") != `17 {"access_token":"test-credential"}` {
				t.Error("wrong credential exchange header")
			}
			if r.Header.Get("User-Agent") != zedUserAgent {
				t.Error("missing credential User-Agent")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": fresh})
		case "/completions":
			completions.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+fresh {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprintln(w, `{"error":{"message":"expired token"}}`)
				return
			}
			_, _ = fmt.Fprintln(w, zedTestTerminal)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	request, options := zedTestRequest()
	if _, errExecute := executor.Execute(context.Background(), auth, request, options); errExecute != nil {
		t.Fatal(errExecute)
	}
	if refreshes.Load() != 1 || completions.Load() != 2 {
		t.Fatalf("wrong retry counts: %d / %d", refreshes.Load(), completions.Load())
	}
	if !executor.ShouldPrepareRequestAuth(auth) {
		t.Fatal("stale persisted token was not detected")
	}
	prepared, errPrepare := executor.PrepareRequestAuth(context.Background(), auth)
	if errPrepare != nil {
		t.Fatal(errPrepare)
	}
	if prepared.Metadata["access_token"] != fresh || executor.ShouldPrepareRequestAuth(prepared) {
		t.Fatal("refreshed token was not reused")
	}
	if refreshes.Load() != 1 {
		t.Fatal("cached refresh caused another exchange")
	}
	if auth.Metadata["access_token"] == fresh {
		t.Fatal("shared original auth was mutated")
	}
}

func TestZedConcurrentPreparationUsesOneExchange(t *testing.T) {
	auth := zedTestAuth()
	delete(auth.Metadata, "access_token")
	auth.Metadata["user_id"] = float64(17)
	var exchanges atomic.Int32
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": zedTestJWT(zedTestNow.Add(time.Hour), "parallel")})
	})
	var workers sync.WaitGroup
	errorsCh := make(chan error, 8)
	for range 8 {
		workers.Go(func() {
			prepared, errPrepare := executor.PrepareRequestAuth(context.Background(), auth)
			if errPrepare != nil {
				errorsCh <- errPrepare
				return
			}
			if executor.ShouldPrepareRequestAuth(prepared) {
				errorsCh <- errors.New("prepared token is not fresh")
			}
		})
	}
	workers.Wait()
	close(errorsCh)
	for errPrepare := range errorsCh {
		t.Error(errPrepare)
	}
	if exchanges.Load() != 1 {
		t.Fatalf("got %d exchanges, want 1", exchanges.Load())
	}
}

func TestZedFailsOnTruncationAndStatusErrors(t *testing.T) {
	cases := []struct {
		name, body    string
		statusSupport bool
		wantCode      int
	}{
		{"EOF without response", `{"type":"response.created","response":{"id":"resp_test"}}` + "\n", false, 0},
		{"negotiated status missing", zedTestTerminal + "\n", true, 0},
		{"ended without response", `{"status":"stream_ended"}` + "\n", true, 0},
		{"malformed frame", "{not-json}\n", false, 0},
		{"plan failed status", `{"status":{"failed":{"code":403,"message":"not in plan","request_id":"request-1"}}}` + "\n", true, 403},
		{"response error", `{"type":"response.error","error":{"code":429,"message":"rate limit","retry_after":2}}` + "\n", false, 429},
		{"response failed", `{"type":"response.failed","response":{"error":{"code":"400","message":"bad request"}}}` + "\n", false, 400},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				if test.statusSupport {
					w.Header().Set("x-zed-server-supports-status-messages", "true")
				}
				_, _ = fmt.Fprint(w, test.body)
			})
			request, options := zedTestRequest()
			response, errExecute := executor.Execute(context.Background(), zedTestAuth(), request, options)
			if errExecute == nil {
				t.Fatal("bad stream was accepted")
			}
			if len(response.Payload) != 0 {
				t.Error("failed nonstream execution returned a result")
			}
			if test.wantCode != 0 {
				var upstream *helps.ZedError
				if !errors.As(errExecute, &upstream) || upstream.Code != test.wantCode {
					t.Fatalf("want status %d, got %v", test.wantCode, errExecute)
				}
			}
			stream, errStream := executor.ExecuteStream(context.Background(), zedTestAuth(), request, options)
			if errStream != nil {
				t.Fatal(errStream)
			}
			var terminalLeaked, gotError bool
			for chunk := range stream.Chunks {
				gotError = gotError || chunk.Err != nil
				terminalLeaked = terminalLeaked || bytes.Contains(chunk.Payload, []byte("response.completed"))
			}
			if terminalLeaked || !gotError {
				t.Fatalf("terminal leaked=%v error=%v", terminalLeaked, gotError)
			}
		})
	}
}

func TestZedCancellationClosesUpstream(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, `{"type":"response.created","response":{"id":"resp_test"}}`)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, options := zedTestRequest()
	stream, errStream := executor.ExecuteStream(ctx, zedTestAuth(), request, options)
	if errStream != nil {
		t.Fatal(errStream)
	}
	<-started
	first := <-stream.Chunks
	if first.Err != nil {
		t.Fatal(first.Err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		for range stream.Chunks {
		}
		close(done)
	}()
	for _, ch := range []<-chan struct{}{done, stopped} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("cancel did not close stream/upstream")
		}
	}
}

func TestZedRejectsUnsupportedFormatsAndModelsBeforeNetwork(t *testing.T) {
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected network request")
		w.WriteHeader(500)
	})
	request, options := zedTestRequest()
	request.Model = "gpt-5.6-terra"
	_, errExecute := executor.Execute(context.Background(), zedTestAuth(), request, options)
	if errExecute == nil || !strings.Contains(errExecute.Error(), "only gpt-5.6-sol") {
		t.Fatalf("unexpected model result: %v", errExecute)
	}
	request, options = zedTestRequest()
	options.SourceFormat = sdktranslator.FormatClaude
	_, errExecute = executor.Execute(context.Background(), zedTestAuth(), request, options)
	if errExecute == nil || !strings.Contains(errExecute.Error(), "native Responses") {
		t.Fatalf("unexpected format result: %v", errExecute)
	}
}

func TestZedExplicitTokenFailureHeadersRefreshOnce(t *testing.T) {
	for _, header := range []string{"x-zed-expired-token", "x-zed-outdated-token"} {
		t.Run(header, func(t *testing.T) {
			var exchanges, completions atomic.Int32
			fresh := zedTestJWT(zedTestNow.Add(time.Hour), header)
			executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/client/llm_tokens" {
					exchanges.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]string{"token": fresh})
					return
				}
				completions.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+fresh {
					w.Header().Set(header, "")
					w.WriteHeader(http.StatusForbidden)
					_, _ = fmt.Fprintln(w, `{"error":{"message":"refresh this token"}}`)
					return
				}
				_, _ = fmt.Fprintln(w, zedTestTerminal)
			})
			request, options := zedTestRequest()
			if _, errExecute := executor.Execute(context.Background(), zedTestAuth(), request, options); errExecute != nil {
				t.Fatal(errExecute)
			}
			if exchanges.Load() != 1 || completions.Load() != 2 {
				t.Fatalf("unexpected exchange/completion counts %d/%d", exchanges.Load(), completions.Load())
			}
		})
	}
}

func TestZedRejectsTokenWithoutJWTExpiry(t *testing.T) {
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "opaque-without-expiry"})
	})
	auth := zedTestAuth()
	auth.Metadata["expired"] = zedTestNow.Add(time.Hour).Format(time.RFC3339)
	_, errRefresh := executor.Refresh(context.Background(), auth)
	if errRefresh == nil {
		t.Fatal("accepted an invalid JWT using the old token's metadata expiry")
	}
}

func TestZedStatusHeaderPresenceRequiresStreamEnded(t *testing.T) {
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-zed-server-supports-status-messages", "")
		_, _ = fmt.Fprintln(w, zedTestTerminal)
	})
	request, options := zedTestRequest()
	_, errExecute := executor.Execute(context.Background(), zedTestAuth(), request, options)
	if errExecute == nil || !strings.Contains(errExecute.Error(), "truncated") {
		t.Fatalf("status header presence was ignored: %v", errExecute)
	}
}

func TestZedMapsOnlyDeveloperMessageRoles(t *testing.T) {
	executor := NewZedExecutor(&config.Config{})
	request, options := zedTestRequest()
	request.Payload = []byte(`{
		"model":"gpt-5.6-sol","stream":true,
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"First instruction","metadata":{"role":"developer"}}],"phase":"commentary"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Task"}]},
			{"type":"message","role":"system","content":[{"type":"input_text","text":"Existing system"}]},
			{"type":"reasoning","encrypted_content":"unchanged","summary":[],"role":"developer"},
			{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}","role":"developer"},
			{"type":"function_call_output","call_id":"call_1","output":"file contents"},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"Second instruction"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Answer"}]}
		],
		"tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}],
		"reasoning":{"effort":"low"},"max_output_tokens":256
	}`)
	original := bytes.Clone(request.Payload)
	payload, errPayload := executor.payload(request, options)
	if errPayload != nil {
		t.Fatal(errPayload)
	}
	if !bytes.Equal(request.Payload, original) {
		t.Fatal("incoming request bytes were mutated")
	}
	var envelope map[string]json.RawMessage
	var expected, got map[string]any
	_ = json.Unmarshal(payload, &envelope)
	_ = json.Unmarshal(original, &expected)
	_ = json.Unmarshal(envelope["provider_request"], &got)
	items := expected["input"].([]any)
	items[0].(map[string]any)["role"] = "system"
	items[6].(map[string]any)["role"] = "system"
	if !reflect.DeepEqual(expected, got) {
		t.Fatalf("unexpected normalization: want %v, got %v", expected, got)
	}
}

func TestZedNormalizesOnlyExplicitNullReasoningContent(t *testing.T) {
	executor := NewZedExecutor(&config.Config{})
	request, options := zedTestRequest()
	request.Payload = []byte(`{
		"model":"gpt-5.6-sol","stream":true,"metadata":null,
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue"}]},
			{"type":"reasoning","id":"rs_1","content":null,"encrypted_content":"opaque-state","summary":[]},
			{"type":"reasoning","id":"rs_2","encrypted_content":"missing-content-unchanged","summary":[]},
			{"type":"reasoning","id":"rs_3","content":[],"encrypted_content":"empty-content-unchanged","summary":[]},
			{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}","content":null},
			{"type":"function_call_output","call_id":"call_1","output":"file contents"},
			{"type":"custom_tool_call_output","call_id":"patch_1","output":"success","content":null}
		],
		"reasoning":{"effort":"low"},"max_output_tokens":256
	}`)
	original := bytes.Clone(request.Payload)
	payload, errPayload := executor.payload(request, options)
	if errPayload != nil {
		t.Fatal(errPayload)
	}
	if !bytes.Equal(original, request.Payload) {
		t.Fatal("incoming request bytes were mutated")
	}
	var envelope map[string]json.RawMessage
	var expected, got map[string]any
	_ = json.Unmarshal(payload, &envelope)
	_ = json.Unmarshal(original, &expected)
	_ = json.Unmarshal(envelope["provider_request"], &got)
	expected["input"].([]any)[1].(map[string]any)["content"] = []any{}
	if !reflect.DeepEqual(expected, got) {
		t.Fatalf("unexpected null normalization: want %v, got %v", expected, got)
	}
}

type zedRefreshLifecycleHook struct {
	cliproxyauth.NoopHook
	updated chan *cliproxyauth.Auth
}

func (h *zedRefreshLifecycleHook) OnAuthUpdated(_ context.Context, auth *cliproxyauth.Auth) {
	if auth.LastRefreshedAt.IsZero() {
		return
	}
	select {
	case h.updated <- auth.Clone():
	default:
	}
}

func TestZedExpiredCredentialAutoRefresh(t *testing.T) {
	testZedCredentialAutoRefresh(t, -time.Hour)
}

func TestZedNearlyExpiredCredentialAutoRefresh(t *testing.T) {
	testZedCredentialAutoRefresh(t, 30*time.Second)
}

func testZedCredentialAutoRefresh(t *testing.T, lifetime time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	auth := zedTestAuth()
	auth.ID = "zed-refresh-" + t.Name()
	auth.Status = cliproxyauth.StatusActive
	auth.Metadata["access_token"] = zedTestJWT(now.Add(lifetime), "before-refresh")
	fresh := zedTestJWT(now.Add(time.Hour), "renewed")
	var exchanges atomic.Int32
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/llm_tokens" {
			t.Error("lifecycle refresh unexpectedly called a model")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		exchanges.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": fresh})
	})
	executor.now = func() time.Time { return now }
	hook := &zedRefreshLifecycleHook{updated: make(chan *cliproxyauth.Auth, 1)}
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, hook)
	manager.RegisterExecutor(executor)
	model := "zed/" + zedSolModel
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "zed", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	if _, errSelect := manager.SelectAuth(ctx, "zed", model, options); (errSelect != nil) != (lifetime <= 0) {
		t.Fatal("pre-refresh selection did not match JWT expiry")
	}
	// Match the daemon's interval. Due credentials must refresh immediately,
	// not wait for either a model request or the fifteen-minute fallback interval.
	manager.StartAutoRefresh(ctx, 15*time.Minute)
	defer manager.StopAutoRefresh()
	select {
	case updated := <-hook.updated:
		if updated.Metadata["access_token"] != fresh {
			t.Fatal("auto-refresh did not install the new JWT")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expired Zed credential was never scheduled for refresh")
	}
	selected, errSelect := manager.SelectAuth(ctx, "zed", model, options)
	if errSelect != nil {
		t.Fatalf("refreshed JWT is still unavailable: %v", errSelect)
	}
	if selected.Metadata["access_token"] != fresh || exchanges.Load() != 1 {
		t.Fatalf("unexpected refresh lifecycle: exchanges=%d", exchanges.Load())
	}
}

func TestZedRepeatedTokenFailuresRemainUnauthorized(t *testing.T) {
	for _, test := range []struct {
		name, header string
		status       int
	}{
		{"HTTP unauthorized", "", http.StatusUnauthorized},
		{"expired forbidden", "x-zed-expired-token", http.StatusForbidden},
		{"outdated forbidden", "x-zed-outdated-token", http.StatusForbidden},
		{"expired success", "x-zed-expired-token", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			var exchanges, completions atomic.Int32
			executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/client/llm_tokens" {
					exchanges.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]string{"token": zedTestJWT(zedTestNow.Add(time.Hour), "renewed")})
					return
				}
				completions.Add(1)
				if test.header != "" {
					w.Header().Set(test.header, "")
				}
				w.WriteHeader(test.status)
				_, _ = fmt.Fprintln(w, `{"error":{"code":403,"message":"expired credential"}}`)
			})
			request, options := zedTestRequest()
			_, errExecute := executor.Execute(context.Background(), zedTestAuth(), request, options)
			var upstream *helps.ZedError
			if !errors.As(errExecute, &upstream) || upstream.StatusCode() != http.StatusUnauthorized {
				t.Fatalf("repeated credential failure lost auth classification: %v", errExecute)
			}
			if exchanges.Load() != 1 || completions.Load() != 2 {
				t.Fatalf("unexpected authentication retries: exchanges=%d completions=%d", exchanges.Load(), completions.Load())
			}
		})
	}
}

func TestZedCancellationWithoutStreamConsumerClosesUpstream(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, `{"type":"response.created","response":{"id":"resp_test"}}`)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	producerClosed := make(chan struct{})
	transport := executor.client.Transport
	executor.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		response, errRoundTrip := transport.RoundTrip(request)
		if errRoundTrip == nil {
			response.Body = &closeSignalReadCloser{ReadCloser: response.Body, closed: producerClosed}
		}
		return response, errRoundTrip
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, options := zedTestRequest()
	stream, errStream := executor.ExecuteStream(ctx, zedTestAuth(), request, options)
	if errStream != nil {
		t.Fatal(errStream)
	}
	<-started
	// No reader drains the event channel while the request is active.
	cancel()
	for _, done := range []<-chan struct{}{producerClosed, stopped} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("cancel did not release the producer/upstream while the consumer was absent")
		}
	}
	drained := make(chan struct{})
	go func() {
		for range stream.Chunks {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled stream did not close its result channel")
	}
}

type captureZedUsagePlugin struct {
	authID  string
	records chan usage.Record
	barrier chan struct{}
}

func (p *captureZedUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	if record.Provider != "zed" || record.AuthID != p.authID {
		return
	}
	if record.Model == "zed-usage-barrier" {
		close(p.barrier)
		return
	}
	p.records <- record
}

func TestZedUsageWaitsForConfirmedOutcome(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, outcome := range []string{"completed", "truncated", "provider failure", "transport failure"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, outcome), func(t *testing.T) {
				auth := zedTestAuth()
				auth.ID = "zed-usage-" + t.Name()
				capture := &captureZedUsagePlugin{authID: auth.ID, records: make(chan usage.Record, 8), barrier: make(chan struct{})}
				usage.RegisterNamedPlugin("zed-usage-regression", capture)
				created := `{"type":"response.created","response":{"id":"resp_test","service_tier":"priority","usage":null}}`
				body := created + "\n" + zedTestTerminal + "\n"
				switch outcome {
				case "completed":
					body += `{"status":"stream_ended"}` + "\n"
				case "provider failure":
					body = created + "\n" + `{"status":{"failed":{"code":429,"message":"rate limited"}}}` + "\n"
				}
				executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("x-zed-server-supports-status-messages", "true")
					if outcome == "transport failure" {
						w.Header().Set("Content-Length", fmt.Sprint(len(body)+10))
					}
					_, _ = fmt.Fprint(w, body)
				})
				request, options := zedTestRequest()
				var failed bool
				if stream {
					result, errStream := executor.ExecuteStream(context.Background(), auth, request, options)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range result.Chunks {
						failed = failed || chunk.Err != nil
					}
				} else {
					_, errExecute := executor.Execute(context.Background(), auth, request, options)
					failed = errExecute != nil
				}
				wantFailed := outcome != "completed"
				if failed != wantFailed {
					t.Fatalf("execution failed=%v, want %v", failed, wantFailed)
				}
				// The usage queue is FIFO. This barrier proves all request
				// records were dispatched without sleeping to guess completion.
				usage.PublishRecord(context.Background(), usage.Record{Provider: "zed", AuthID: auth.ID, Model: "zed-usage-barrier"})
				select {
				case <-capture.barrier:
				case <-time.After(5 * time.Second):
					t.Fatal("usage records were not dispatched")
				}
				if len(capture.records) != 1 {
					t.Fatalf("got %d usage records, want one", len(capture.records))
				}
				record := <-capture.records
				if record.Failed != wantFailed {
					t.Fatalf("usage failed=%v, want %v", record.Failed, wantFailed)
				}
				if record.ResponseServiceTier != "priority" {
					t.Fatalf("initial service tier lost: %q", record.ResponseServiceTier)
				}
				if !wantFailed {
					detail := record.Detail
					if detail.InputTokens != 21 || detail.OutputTokens != 9 || detail.TotalTokens != 30 ||
						detail.CachedTokens != 7 || detail.ReasoningTokens != 4 {
						t.Fatalf("terminal usage was lost: %+v", detail)
					}
				} else if outcome == "provider failure" && record.Fail.StatusCode != 429 {
					t.Fatalf("failure status lost: %d", record.Fail.StatusCode)
				}
			})
		}
	}
}

func TestZedFreshJWTWinsOverStaleLegacyExpiryMetadata(t *testing.T) {
	auth := zedTestAuth()
	for _, key := range []string{"expired", "expire", "expires_at", "expiresAt", "expiry", "expires"} {
		auth.Metadata[key] = zedTestNow.Add(-time.Hour).Format(time.RFC3339)
	}
	expiry := zedTestNow.Add(time.Hour)
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": zedTestJWT(expiry, "fresh-alias-test")})
	})
	updated, errRefresh := executor.Refresh(context.Background(), auth)
	if errRefresh != nil {
		t.Fatal(errRefresh)
	}
	if got, ok := updated.ExpirationTime(); !ok || !got.Equal(expiry) {
		t.Fatal("stale metadata overrode the fresh JWT expiry")
	}
	if !updated.HasValidAccessToken(zedTestNow) || executor.ShouldPrepareRequestAuth(updated) {
		t.Fatal("the fresh JWT was rejected or immediately refreshed")
	}
}

func TestZedMarksOnlyInferenceTransportAttempts(t *testing.T) {
	for _, scenario := range []string{"invalid model", "credential failure", "inference failure"} {
		t.Run(scenario, func(t *testing.T) {
			auth := zedTestAuth()
			request, options := zedTestRequest()
			if scenario == "invalid model" {
				request.Model = "unsupported"
			}
			if scenario == "credential failure" {
				delete(auth.Metadata, "access_token")
			}
			executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/client/llm_tokens" {
					w.WriteHeader(http.StatusUnauthorized)
				} else {
					w.WriteHeader(http.StatusForbidden)
				}
				_, _ = fmt.Fprintln(w, `{"error":{"message":"rejected"}}`)
			})
			ctx := cliproxyexecutor.WithUpstreamAttemptTracker(context.Background())
			if _, errExecute := executor.Execute(ctx, auth, request, options); errExecute == nil {
				t.Fatal("test failure response was accepted")
			}
			if got, want := cliproxyexecutor.UpstreamAttempted(ctx), scenario == "inference failure"; got != want {
				t.Fatalf("upstream attempted=%v, want %v", got, want)
			}
		})
	}
}
