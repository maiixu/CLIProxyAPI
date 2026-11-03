package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestZedSlowExchangeDoesNotBlockOtherAccounts(t *testing.T) {
	slow := zedTestAuth()
	slow.ID = "zed-slow.json"
	slow.Metadata["user_id"] = "18"
	slow.Metadata["credential"] = map[string]any{"access_token": "slow-credential"}
	delete(slow.Metadata, "access_token")
	other := zedTestAuth()
	otherToken := other.Metadata["access_token"].(string)

	exchangeStarted := make(chan struct{})
	releaseExchange := make(chan struct{})
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client/llm_tokens":
			if r.Header.Get("Authorization") != `18 {"access_token":"slow-credential"}` {
				t.Errorf("unexpected exchange for %q", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			close(exchangeStarted)
			<-releaseExchange
			_ = json.NewEncoder(w).Encode(map[string]string{"token": zedTestJWT(zedTestNow.Add(time.Hour), "slow")})
		case "/completions":
			if r.Header.Get("Authorization") != "Bearer "+otherToken {
				t.Errorf("unexpected completion token")
			}
			_, _ = fmt.Fprintln(w, zedTestTerminal)
		}
	})

	slowDone := make(chan error, 1)
	go func() {
		_, errPrepare := executor.PrepareRequestAuth(context.Background(), slow)
		slowDone <- errPrepare
	}()
	select {
	case <-exchangeStarted:
	case <-time.After(5 * time.Second):
		close(releaseExchange)
		t.Fatal("slow exchange never started")
	}

	otherDone := make(chan error, 1)
	go func() {
		// Both the per-request preparation check and the request itself must
		// proceed while the other account's exchange is in flight.
		if executor.ShouldPrepareRequestAuth(other) {
			otherDone <- fmt.Errorf("fresh account unexpectedly needs preparation")
			return
		}
		request, options := zedTestRequest()
		_, errExecute := executor.Execute(context.Background(), other, request, options)
		otherDone <- errExecute
	}()
	select {
	case errOther := <-otherDone:
		if errOther != nil {
			close(releaseExchange)
			t.Fatal(errOther)
		}
	case <-time.After(5 * time.Second):
		close(releaseExchange)
		t.Fatal("request for another account was blocked by a slow token exchange")
	}

	close(releaseExchange)
	if errSlow := <-slowDone; errSlow != nil {
		t.Fatal(errSlow)
	}
}

func TestZedTransientExchangeUnauthorizedDoesNotSidelineAccount(t *testing.T) {
	now := time.Now().UTC()
	auth := zedTestAuth()
	auth.ID = "zed-transient-exchange.json"
	auth.Status = cliproxyauth.StatusActive
	auth.Metadata["access_token"] = zedTestJWT(now.Add(time.Hour), "original")
	fresh := zedTestJWT(now.Add(2*time.Hour), "renewed")
	var exchanges, completions atomic.Int32
	executor := zedTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/client/llm_tokens":
			if exchanges.Add(1) == 1 {
				// One transient rejection from the token-exchange endpoint.
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprintln(w, `{"error":{"message":"try again"}}`)
				return
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
		}
	})
	executor.now = func() time.Time { return now }
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	model := "zed/" + zedSolModel
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "zed", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	ctx := context.Background()
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatal(errRegister)
	}

	request, _ := zedTestRequest()
	request.Model = model
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	if _, errExecute := manager.Execute(ctx, []string{"zed"}, request, options); errExecute != nil {
		t.Fatalf("transient exchange 401 was not recovered: %v", errExecute)
	}
	if exchanges.Load() != 2 {
		t.Fatalf("got %d exchanges, want 2", exchanges.Load())
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth disappeared")
	}
	if current.Unavailable || current.NextRetryAfter.After(time.Now()) {
		t.Fatalf("account sidelined after a recovered exchange 401: unavailable=%v next_retry=%v", current.Unavailable, current.NextRetryAfter)
	}
	for _, state := range current.ModelStates {
		if state != nil && (state.Unavailable || state.NextRetryAfter.After(time.Now())) {
			t.Fatalf("model sidelined after a recovered exchange 401: %+v", state)
		}
	}
	if _, errSelect := manager.SelectAuth(ctx, "zed", model, options); errSelect != nil {
		t.Fatalf("account is not selectable after recovery: %v", errSelect)
	}
	if _, errExecute := manager.Execute(ctx, []string{"zed"}, request, options); errExecute != nil {
		t.Fatalf("follow-up request failed: %v", errExecute)
	}
}
