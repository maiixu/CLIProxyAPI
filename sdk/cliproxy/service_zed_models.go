package cliproxy

import "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"

// zedModels exposes only the verified Responses model with Zed's token limits.
func zedModels() []*ModelInfo {
	for _, model := range registry.GetCodexProModels() {
		if model == nil || model.ID != "gpt-5.6-sol" {
			continue
		}
		// Registry getters return copies, so these limits do not affect Codex.
		model.InputTokenLimit = 272000
		model.OutputTokenLimit = 128000
		model.ContextLength = 400000
		model.MaxCompletionTokens = 128000
		// Codex uses this advertised window to budget the next request's input.
		model.MaxContextLength = model.InputTokenLimit
		return []*ModelInfo{model}
	}
	return nil
}
