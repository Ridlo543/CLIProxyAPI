package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/antigravity"
	"github.com/tidwall/gjson"
)

// The policy lives in config.yaml but is enforced inside the thinking package;
// this checks the hand-off the builder and every config reload perform.
func TestApplyReasoningPolicyReachesTheRequestAndClearsOnReload(t *testing.T) {
	t.Cleanup(func() { thinking.SetReasoningPolicy(nil) })
	modelInfo := &registry.ModelInfo{
		ID:                  "claude-opus-4-6-thinking",
		Type:                "antigravity",
		MaxCompletionTokens: 64000,
		Thinking:            &registry.ThinkingSupport{Min: 1024, Max: 64000, ZeroAllowed: true, DynamicAllowed: true},
	}
	send := func() []byte {
		out, err := thinking.ApplyThinkingWithModelInfo([]byte(`{"request":{"generationConfig":{}}}`), []byte(`{}`),
			"claude-opus-4-6-thinking", "openai", "antigravity", "antigravity", modelInfo)
		if err != nil {
			t.Fatalf("ApplyThinkingWithModelInfo() error = %v", err)
		}
		return out
	}

	applyReasoningPolicy(&config.Config{ReasoningPolicy: map[string]config.ReasoningProviderPolicy{
		"antigravity": {Models: map[string]string{"claude-opus-4-6-thinking": "8000"}},
	}})
	if got := gjson.GetBytes(send(), "request.generationConfig.thinkingConfig.thinkingBudget").Int(); got != 8000 {
		t.Fatalf("thinkingBudget = %d, want 8000 from config", got)
	}

	// A reload that removed the section must stop forcing it.
	applyReasoningPolicy(&config.Config{})
	if out := send(); gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("policy still applied after it was removed from config; body=%s", out)
	}
}
