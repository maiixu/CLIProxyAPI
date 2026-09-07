package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	zedUpstreamURL   = "https://cloud.zed.dev"
	zedSolModel      = "gpt-5.6-sol"
	zedUserAgent     = "Zed/0.222.4"
	zedClientVersion = "0.222.4+stable.147.b385025df963c9e8c3f74cc4dadb1c4b29b3c6f0"
)

// ZedExecutor forwards native Responses requests through a Zed account.
// The private transport fields are test seams, never credential/config overrides.
type ZedExecutor struct {
	cfg       *config.Config
	baseURL   string
	client    *http.Client
	now       func() time.Time
	refreshMu sync.Mutex
	tokens    map[[32]byte]*cliproxyauth.Auth
}

func NewZedExecutor(cfg *config.Config) *ZedExecutor {
	// Selection rejects expired JWTs before request preparation. Register the
	// account refresh policy so the manager renews them before that boundary.
	cliproxyauth.RegisterRefreshLeadProvider("zed", func() *time.Duration {
		lead := time.Minute
		return &lead
	})
	return &ZedExecutor{
		cfg: cfg, baseURL: zedUpstreamURL, now: time.Now,
		tokens: make(map[[32]byte]*cliproxyauth.Auth),
	}
}

func (e *ZedExecutor) Identifier() string { return "zed" }

func (e *ZedExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAIResponse
}

func (e *ZedExecutor) token(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	token, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(token)
}

func (e *ZedExecutor) fresh(auth *cliproxyauth.Auth) bool {
	if e.token(auth) == "" {
		return false
	}
	expiry, ok := auth.ExpirationTime()
	return ok && expiry.After(e.now().Add(time.Minute))
}

func (e *ZedExecutor) credentials(auth *cliproxyauth.Auth) (string, [32]byte, error) {
	if auth == nil || auth.Metadata == nil {
		return "", [32]byte{}, fmt.Errorf("zed: account credentials are missing")
	}
	var userID string
	switch value := auth.Metadata["user_id"].(type) {
	case string:
		userID = strings.TrimSpace(value)
	case json.Number:
		userID = value.String()
	case float64:
		if value == float64(int64(value)) {
			userID = strconv.FormatInt(int64(value), 10)
		}
	}
	if userID == "" || strings.ContainsAny(userID, " \t\r\n") {
		return "", [32]byte{}, fmt.Errorf("zed: account user_id is invalid")
	}
	credential, errMarshal := json.Marshal(auth.Metadata["credential"])
	if errMarshal != nil || len(credential) == 0 || credential[0] != '{' {
		return "", [32]byte{}, fmt.Errorf("zed: account credential must be a JSON object")
	}
	header := userID + " " + string(credential)
	return header, sha256.Sum256([]byte(header)), nil
}

func (e *ZedExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	if !e.fresh(auth) {
		return true
	}
	_, key, errCredentials := e.credentials(auth)
	if errCredentials != nil {
		return true
	}
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	cached := e.tokens[key]
	return e.fresh(cached) && e.token(cached) != e.token(auth)
}

func (e *ZedExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return e.refresh(ctx, auth, "")
}

func (e *ZedExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return e.refresh(ctx, auth, "")
}

// refresh serializes exchanges, including a retry after an actual HTTP 401.
// RequestAuthPreparer persists this snapshot; concurrent requests reuse its JWT.
func (e *ZedExecutor) refresh(ctx context.Context, auth *cliproxyauth.Auth, rejectedToken string) (*cliproxyauth.Auth, error) {
	header, key, errCredentials := e.credentials(auth)
	if errCredentials != nil {
		return nil, errCredentials
	}
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	if cached := e.tokens[key]; e.fresh(cached) && e.token(cached) != rejectedToken {
		updated := auth.Clone()
		if updated.Metadata == nil {
			updated.Metadata = make(map[string]any)
		}
		for _, field := range []string{"access_token", "expires_at", "system_id"} {
			updated.Metadata[field] = cached.Metadata[field]
		}
		return updated, nil
	}

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	systemID, _ := updated.Metadata["system_id"].(string)
	if systemID == "" {
		systemID = uuid.NewString()
	}
	credentialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, errRequest := http.NewRequestWithContext(credentialCtx, http.MethodPost, e.baseURL+"/client/llm_tokens", http.NoBody)
	if errRequest != nil {
		return nil, fmt.Errorf("zed: create credential request: %w", errRequest)
	}
	request.Header.Set("Authorization", header)
	e.setClientHeaders(request)
	request.Header.Set("x-zed-system-id", systemID)
	response, errDo := e.httpClient(ctx, auth).Do(request)
	if errDo != nil {
		return nil, fmt.Errorf("zed: credential exchange: %w", errDo)
	}
	defer e.closeBody(response.Body)
	body, errRead := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("zed: read credential response: %w", errRead)
	}
	if response.StatusCode != http.StatusOK {
		return nil, helps.NewZedError(response.StatusCode, body)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil || strings.TrimSpace(payload.Token) == "" {
		return nil, fmt.Errorf("zed: credential exchange returned no token")
	}
	updated.Metadata["access_token"] = payload.Token
	// The fresh JWT must supply an expiry; never treat an unknown token lifetime as permanent.
	delete(updated.Metadata, "expires_at")
	tokenOnly := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": payload.Token}}
	expiry, ok := tokenOnly.AccessTokenExpirationTime()
	if !ok || !expiry.After(e.now().Add(time.Minute)) {
		return nil, fmt.Errorf("zed: credential exchange returned an expired or invalid token")
	}
	updated.Metadata["expires_at"] = expiry.UTC().Format(time.RFC3339)
	updated.Metadata["system_id"] = systemID
	if e.tokens == nil {
		e.tokens = make(map[[32]byte]*cliproxyauth.Auth)
	}
	e.tokens[key] = updated.Clone()
	return updated, nil
}

func (e *ZedExecutor) httpClient(ctx context.Context, auth *cliproxyauth.Auth) *http.Client {
	if e.client != nil {
		return e.client
	}
	client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	// Credentials must never follow an upstream redirect to another origin.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func (e *ZedExecutor) setClientHeaders(request *http.Request) {
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", zedUserAgent)
	request.Header.Set("x-zed-version", zedClientVersion)
}

func (e *ZedExecutor) closeBody(body io.Closer) {
	if errClose := body.Close(); errClose != nil {
		log.WithError(errClose).Debug("zed: close upstream response")
	}
}

func (e *ZedExecutor) payload(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) ([]byte, error) {
	if opts.Alt != "" {
		return nil, &helps.ZedError{Code: http.StatusNotImplemented, Message: "alternate Responses endpoints are not supported"}
	}
	if opts.SourceFormat != sdktranslator.FormatOpenAIResponse && opts.SourceFormat != sdktranslator.FormatCodex {
		return nil, &helps.ZedError{Code: http.StatusBadRequest, Message: "Zed Sol requires the native Responses protocol"}
	}
	target := cliproxyexecutor.ResponseFormatOrSource(opts)
	if target != sdktranslator.FormatOpenAIResponse && target != sdktranslator.FormatCodex {
		return nil, &helps.ZedError{Code: http.StatusBadRequest, Message: "Zed Sol requires native Responses output"}
	}
	model := strings.TrimPrefix(strings.TrimSpace(req.Model), "zed/")
	if model != zedSolModel {
		return nil, &helps.ZedError{Code: http.StatusBadRequest, Message: "this Zed provider supports only gpt-5.6-sol"}
	}
	var input map[string]json.RawMessage
	if errDecode := json.Unmarshal(req.Payload, &input); errDecode != nil || input == nil {
		return nil, &helps.ZedError{Code: http.StatusBadRequest, Message: "invalid Responses request"}
	}
	// Do not run Codex's OAuth converter: it removes output limits and API fields.
	providerRequest, errModel := sjson.SetBytes(req.Payload, "model", model)
	if errModel != nil {
		return nil, errModel
	}
	providerRequest, errStream := sjson.SetBytes(providerRequest, "stream", true)
	if errStream != nil {
		return nil, errStream
	}

	// Normalize only the two schema differences observed in native Codex requests.
	if items := gjson.GetBytes(req.Payload, "input"); items.IsArray() {
		for index, item := range items.Array() {
			if item.Get("type").String() == "message" && item.Get("role").String() == "developer" {
				// Zed uses the legacy system role for developer messages.
				var errRole error
				providerRequest, errRole = sjson.SetBytes(providerRequest, fmt.Sprintf("input.%d.role", index), "system")
				if errRole != nil {
					return nil, errRole
				}
			}
			if content := item.Get("content"); item.Get("type").String() == "reasoning" && content.Exists() && content.Type == gjson.Null {
				// Zed's reasoning content is a sequence; preserve the opaque state.
				var errContent error
				providerRequest, errContent = sjson.SetRawBytes(providerRequest, fmt.Sprintf("input.%d.content", index), []byte("[]"))
				if errContent != nil {
					return nil, errContent
				}
			}
		}
	}

	sessionID, _ := opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string)
	if sessionID == "" {
		sessionID = opts.Headers.Get("Session_id")
	}
	if sessionID == "" {
		sessionID = opts.Headers.Get("X-Session-Id")
	}
	threadID := uuid.NewString()
	if sessionID != "" {
		threadID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("zed:"+sessionID)).String()
	}
	envelope := struct {
		ThreadID string          `json:"thread_id"`
		PromptID string          `json:"prompt_id"`
		Provider string          `json:"provider"`
		Model    string          `json:"model"`
		Request  json.RawMessage `json:"provider_request"`
	}{threadID, uuid.NewString(), "open_ai", model, providerRequest}
	return json.Marshal(envelope)
}

func (e *ZedExecutor) open(ctx context.Context, auth *cliproxyauth.Auth, payload []byte) (*http.Response, error) {
	current := auth
	if e.ShouldPrepareRequestAuth(current) {
		var errPrepare error
		current, errPrepare = e.PrepareRequestAuth(ctx, current)
		if errPrepare != nil {
			return nil, errPrepare
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/completions", bytes.NewReader(payload))
		if errRequest != nil {
			return nil, errRequest
		}
		e.setClientHeaders(request)
		request.Header.Set("Authorization", "Bearer "+e.token(current))
		request.Header.Set("x-zed-client-supports-status-messages", "true")
		request.Header.Set("x-zed-client-supports-stream-ended-request-completion-status", "true")
		cliproxyexecutor.MarkUpstreamAttempt(ctx)
		response, errDo := e.httpClient(ctx, current).Do(request)
		if errDo != nil {
			return nil, fmt.Errorf("zed: completion request: %w", errDo)
		}
		needsRefresh := response.StatusCode == http.StatusUnauthorized ||
			len(response.Header.Values("x-zed-expired-token")) > 0 ||
			len(response.Header.Values("x-zed-outdated-token")) > 0
		if response.StatusCode == http.StatusOK && !needsRefresh {
			return response, nil
		}
		body, errRead := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		e.closeBody(response.Body)
		if errRead != nil {
			return nil, fmt.Errorf("zed: read upstream error: %w", errRead)
		}
		// A plan restriction alone is not an auth failure. Zed also signals
		// outdated credentials through explicit headers, including on HTTP 403.
		if needsRefresh && attempt == 0 {
			var errRefresh error
			current, errRefresh = e.refresh(ctx, current, e.token(current))
			if errRefresh != nil {
				return nil, errRefresh
			}
			continue
		}
		if response.StatusCode == http.StatusOK {
			return nil, &helps.ZedError{Code: http.StatusUnauthorized, Message: "upstream rejected the refreshed token"}
		}
		upstreamError := helps.NewZedError(response.StatusCode, body)
		if needsRefresh {
			// Explicit token rejection remains an auth failure after the one
			// refresh attempt, even when Zed's transport/body reports 403.
			upstreamError.Code = http.StatusUnauthorized
		}
		return nil, upstreamError
	}
	return nil, fmt.Errorf("zed: authentication retry exhausted")
}

// consume preserves every Responses event and requires a confirmed terminal response.
func (e *ZedExecutor) consume(ctx context.Context, response *http.Response, emit func(helps.ZedFrame) error) error {
	defer e.closeBody(response.Body)
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 50*1024*1024)
	requiresEnd := len(response.Header.Values("x-zed-server-supports-status-messages")) > 0
	var terminal *helps.ZedFrame
	for scanner.Scan() {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		frame, errDecode := helps.DecodeZedFrame(line)
		if errDecode != nil {
			return errDecode
		}
		if frame.Ended {
			if terminal == nil {
				return fmt.Errorf("zed: stream ended before a terminal Responses event")
			}
			return emit(*terminal)
		}
		if len(frame.Event) == 0 {
			continue
		}
		if terminal != nil {
			return fmt.Errorf("zed: event received after terminal Responses event")
		}
		if frame.Type == "response.completed" || frame.Type == "response.incomplete" {
			terminal = &frame
			continue
		}
		if errEmit := emit(frame); errEmit != nil {
			return errEmit
		}
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	if errScan := scanner.Err(); errScan != nil {
		return fmt.Errorf("zed: completion stream: %w", errScan)
	}
	if terminal == nil || requiresEnd {
		return fmt.Errorf("zed: truncated completion stream")
	}
	return emit(*terminal)
}

func (e *ZedExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (result cliproxyexecutor.Response, err error) {
	payload, errPayload := e.payload(req, opts)
	if errPayload != nil {
		return result, errPayload
	}
	response, errOpen := e.open(ctx, auth, payload)
	if errOpen != nil {
		return result, errOpen
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, zedSolModel, auth)
	var usageBuffer helps.StreamUsageBuffer
	err = e.consume(ctx, response, func(frame helps.ZedFrame) error {
		if len(frame.Response) > 0 {
			result.Payload = bytes.Clone(frame.Response)
		}
		usageBuffer.Observe(helps.ParseCodexUsage(frame.Event))
		return nil
	})
	if err != nil {
		usageBuffer.PublishFailure(ctx, reporter, err)
		return cliproxyexecutor.Response{}, err
	}
	// consume releases the terminal event only after validating the stream end.
	usageBuffer.Publish(ctx, reporter)
	reporter.EnsurePublished(ctx)
	result.Headers = response.Header.Clone()
	return result, nil
}

func (e *ZedExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	payload, errPayload := e.payload(req, opts)
	if errPayload != nil {
		return nil, errPayload
	}
	response, errOpen := e.open(ctx, auth, payload)
	if errOpen != nil {
		return nil, errOpen
	}
	chunks := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		reporter := helps.NewExecutorUsageReporter(ctx, e, zedSolModel, auth)
		var usageBuffer helps.StreamUsageBuffer
		errConsume := e.consume(ctx, response, func(frame helps.ZedFrame) error {
			usageBuffer.Observe(helps.ParseCodexUsage(frame.Event))
			payload := append([]byte("data: "), frame.Event...)
			payload = append(payload, '\n', '\n')
			select {
			case chunks <- cliproxyexecutor.StreamChunk{Payload: payload}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if errConsume != nil {
			usageBuffer.PublishFailure(ctx, reporter, errConsume)
			select {
			case chunks <- cliproxyexecutor.StreamChunk{Err: errConsume}:
			case <-ctx.Done():
			}
			return
		}
		usageBuffer.Publish(ctx, reporter)
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: response.Header.Clone(), Chunks: chunks}, nil
}

func (e *ZedExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &helps.ZedError{Code: http.StatusNotImplemented, Message: "Zed token counting is not supported"}
}

func (e *ZedExecutor) HttpRequest(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &helps.ZedError{Code: http.StatusNotImplemented, Message: "raw Zed HTTP requests are not supported"}
}
