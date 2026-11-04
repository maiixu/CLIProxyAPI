package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestZedManagerCompletionUnauthorizedStaysCooledDown(t *testing.T) {
	var exchanges, completions atomic.Int32
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client/llm_tokens":
			n := exchanges.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": zedTestJWT(time.Now().Add(time.Hour), fmt.Sprintf("exchange-%d", n))})
		case "/completions":
			completions.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	executor.now = time.Now
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{})
	manager.RegisterExecutor(executor)
	auth := zedTestAuth()
	auth.ID = fmt.Sprintf("zed-revoked-%d.json", time.Now().UnixNano())
	auth.Metadata["access_token"] = zedTestJWT(time.Now().Add(time.Hour), "original")
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "zed", []*registry.ModelInfo{{ID: zedSolModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	request, options := zedTestRequest()
	request.Model = zedSolModel
	_, errExecute := manager.Execute(context.Background(), []string{"zed"}, request, options)
	if errExecute == nil {
		t.Fatal("expected the refused account to fail")
	}
	// One refresh inside the executor; the manager must not refresh and retry again.
	if got := exchanges.Load(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1", got)
	}
	if got := completions.Load(); got != 2 {
		t.Fatalf("completion attempts = %d, want 2", got)
	}

	stored, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth disappeared")
	}
	now := time.Now()
	if !stored.Unavailable || !stored.NextRetryAfter.After(now.Add(25*time.Minute)) {
		t.Fatalf("auth not cooled down: unavailable=%v next_retry=%v", stored.Unavailable, stored.NextRetryAfter)
	}
	// A scheduled refresh would mint a new token and lift the cooldown.
	if stored.NextRefreshAfter.Before(stored.NextRetryAfter) {
		t.Fatalf("refresh scheduled at %v, before cooldown end %v", stored.NextRefreshAfter, stored.NextRetryAfter)
	}

	// The cooled account is not selected again.
	_, errSecond := manager.Execute(context.Background(), []string{"zed"}, request, options)
	if errSecond == nil {
		t.Fatal("expected the cooled account to stay unavailable")
	}
	if got := completions.Load(); got != 2 {
		t.Fatalf("cooled account reached upstream again: completions = %d", got)
	}
}
