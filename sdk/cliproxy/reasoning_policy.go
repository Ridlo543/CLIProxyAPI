package cliproxy

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
)

// applyReasoningPolicy publishes the configured reasoning-policy to the thinking
// pipeline. It runs at build time and on every committed config reload.
func applyReasoningPolicy(cfg *config.Config) {
	if cfg == nil || len(cfg.ReasoningPolicy) == 0 {
		thinking.SetReasoningPolicy(nil)
		return
	}
	policies := make(map[string]thinking.ProviderPolicy, len(cfg.ReasoningPolicy))
	for provider, policy := range cfg.ReasoningPolicy {
		policies[provider] = thinking.ProviderPolicy{Default: policy.Default, Models: policy.Models}
	}
	thinking.SetReasoningPolicy(policies)
}
