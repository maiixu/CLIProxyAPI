package cliproxy

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestZedAuthFileRegistersNativeExecutorAndPrefixedSol(t *testing.T) {
	cfg := &config.Config{}
	cfg.ForceModelPrefix = true
	authDir := t.TempDir()
	auths, err := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{
		Config:  cfg,
		AuthDir: authDir,
		Now:     time.Unix(1, 0),
	}, filepath.Join(authDir, "zed-registration-test.json"), []byte(`{"type":"zed","prefix":"zed","user_id":"17","credential":{"access_token":"test-credential"}}`))
	if err != nil || len(auths) != 1 {
		t.Fatalf("synthesize Zed auth: count=%d err=%v", len(auths), err)
	}
	auth := auths[0]
	if auth.Provider != "zed" || auth.Prefix != "zed" {
		t.Fatalf("auth routing = %q/%q, want zed/zed", auth.Provider, auth.Prefix)
	}
	if _, _, isCompat := openAICompatInfoFromAuth(auth); isCompat {
		t.Fatal("Zed auth was classified as OpenAI compatibility")
	}
	service := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	service.ensureExecutorsForAuth(auth)
	registeredExecutor, ok := service.coreManager.Executor("zed")
	if !ok {
		t.Fatal("Zed executor was not registered")
	}
	if _, ok := registeredExecutor.(*runtimeexecutor.ZedExecutor); !ok {
		t.Fatalf("Zed executor = %T, want native ZedExecutor", registeredExecutor)
	}

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })
	codexModelsBefore := registry.GetCodexProModels()
	service.registerModelsForAuth(context.Background(), auth)
	models := modelRegistry.GetModelsForClient(auth.ID)
	if len(models) != 1 || models[0].ID != "zed/gpt-5.6-sol" {
		t.Fatalf("registered models = %#v, want only zed/gpt-5.6-sol", models)
	}
	model := models[0]
	if modelRegistry.ClientSupportsModel(auth.ID, "gpt-5.6-sol") {
		t.Fatal("forced-prefix auth also supports the unprefixed Sol model")
	}
	if providers := modelRegistry.GetModelProviders(model.ID); !slices.Equal(providers, []string{"zed"}) {
		t.Fatalf("model providers = %v, want [zed]", providers)
	}
	if model.InputTokenLimit != 272000 || model.ContextLength != 400000 || model.MaxCompletionTokens != 128000 || model.OutputTokenLimit != 128000 {
		t.Fatalf("unexpected Zed token limits: %#v", model)
	}
	if !slices.Contains(model.SupportedParameters, "tools") || model.Thinking == nil {
		t.Fatal("Zed Sol lost tool or reasoning capabilities")
	}
	if !reflect.DeepEqual(codexModelsBefore, registry.GetCodexProModels()) {
		t.Fatal("Zed registration changed the shared Codex model definitions")
	}

	var advertised []map[string]any
	for _, available := range modelRegistry.GetAvailableModels("openai") {
		if available["id"] == model.ID {
			advertised = append(advertised, available)
		}
	}
	response := codexmodels.BuildResponse(advertised, modelRegistry.GetModelProviders, false)
	catalog, ok := response["models"].([]map[string]any)
	if !ok || len(catalog) != 1 {
		t.Fatalf("Codex model catalog = %#v", response)
	}
	if catalog[0]["slug"] != model.ID || catalog[0]["context_window"] != 272000 || catalog[0]["max_context_window"] != 272000 {
		t.Fatalf("Codex catalog does not respect Zed input limit: %#v", catalog[0])
	}

	auth.Attributes["excluded_models"] = "gpt-5.6-sol"
	service.registerModelsForAuth(context.Background(), auth)
	if got := modelRegistry.GetModelsForClient(auth.ID); len(got) != 0 {
		t.Fatalf("excluded Zed model remains registered: %#v", got)
	}
	delete(auth.Attributes, "excluded_models")
	service.registerModelsForAuth(context.Background(), auth)
	auth.Disabled = true
	service.registerModelsForAuth(context.Background(), auth)
	if got := modelRegistry.GetModelsForClient(auth.ID); len(got) != 0 {
		t.Fatalf("disabled Zed auth still advertises models: %#v", got)
	}
}

func TestZedExecutorIncludedInBaseline(t *testing.T) {
	service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	service.registerAvailableExecutors(context.Background(), executorRegistrationOptions{includeBaseline: true})
	registered, ok := service.coreManager.Executor("zed")
	if !ok {
		t.Fatal("baseline did not register Zed")
	}
	if _, ok := registered.(*runtimeexecutor.ZedExecutor); !ok {
		t.Fatalf("baseline Zed executor = %T, want native ZedExecutor", registered)
	}
}
