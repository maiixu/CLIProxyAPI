package cliproxy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// Embed the native interface but intercept execution so selection tests never
// authenticate or send inference traffic to Zed.
type zedSelectionRecorder struct {
	*runtimeexecutor.ZedExecutor
	calls int
}

func (e *zedSelectionRecorder) Execute(_ context.Context, _ *coreauth.Auth, _ executor.Request, _ executor.Options) (executor.Response, error) {
	e.calls++
	return executor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *zedSelectionRecorder) ShouldPrepareRequestAuth(_ *coreauth.Auth) bool { return false }

func (e *zedSelectionRecorder) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}

func TestZedFileAuthSelectionRejectsExpiredJWT(t *testing.T) {
	for _, expired := range []bool{true, false} {
		name := "fresh"
		expiry := "2100-01-01T00:00:00Z"
		token := "eyJhbGciOiJub25lIn0.eyJleHAiOiA0MTAyNDQ0ODAwfQ.test" // gitleaks:allow synthetic unsigned JWT for expiry selection test
		if expired {
			name = "expired"
			expiry = "2020-01-01T00:00:00Z"
			token = "eyJhbGciOiJub25lIn0.eyJleHAiOiAxNTc3ODM2ODAwfQ.test" // gitleaks:allow synthetic unsigned JWT for expiry selection test
		}
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.ForceModelPrefix = true
			dir := t.TempDir()
			fixture := strings.ReplaceAll(strings.ReplaceAll(`{"type":"zed","prefix":"zed","user_id":"17","credential":{"access_token":"test-credential"},"disabled":false,"access_token":"TOKEN","expires_at":"EXPIRY"}`, "TOKEN", token), "EXPIRY", expiry)
			auths, err := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{Config: cfg, AuthDir: dir, Now: time.Now()}, filepath.Join(dir, "zed-selection.json"), []byte(fixture))
			if err != nil || len(auths) != 1 {
				t.Fatalf("synthesis: %v", err)
			}
			service := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auths[0].ID) })
			service.applyCoreAuthAddOrUpdate(context.Background(), auths[0])
			recorder := &zedSelectionRecorder{ZedExecutor: runtimeexecutor.NewZedExecutor(cfg)}
			service.coreManager.RegisterExecutor(recorder)
			_, err = service.coreManager.Execute(context.Background(), []string{"zed"}, executor.Request{Model: "zed/gpt-5.6-sol", Payload: []byte(`{"model":"zed/gpt-5.6-sol"}`)}, executor.Options{})
			if expired {
				if err == nil || !strings.Contains(err.Error(), "auth_unavailable") || recorder.calls != 0 {
					t.Fatalf("expired JWT should fail before execution: calls=%d err=%v", recorder.calls, err)
				}
			} else {
				if err != nil || recorder.calls != 1 {
					t.Fatalf("fresh JWT should reach executor once: calls=%d err=%v", recorder.calls, err)
				}
			}
		})
	}
}
