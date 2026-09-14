package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ReasoningProviderPolicy forces a thinking setting on every request served by
// one provider. Values use the model-suffix grammar: auto, none, minimal, low,
// medium, high, xhigh, max, or a token budget. A Models entry wins over Default,
// and both win over whatever the client sent. A provider with no policy leaves
// requests exactly as the client sent them.
type ReasoningProviderPolicy struct {
	Default string            `yaml:"default,omitempty" json:"default,omitempty"`
	Models  map[string]string `yaml:"models,omitempty" json:"models,omitempty"`
}

// NormalizeReasoningPolicy lowercases provider and model keys, drops empty
// entries, and rejects values the thinking pipeline could not parse, so a typo
// fails the config write instead of being silently ignored at request time.
func NormalizeReasoningPolicy(policies map[string]ReasoningProviderPolicy) (map[string]ReasoningProviderPolicy, error) {
	if len(policies) == 0 {
		return nil, nil
	}
	out := make(map[string]ReasoningProviderPolicy, len(policies))
	for rawProvider, policy := range policies {
		provider := strings.ToLower(strings.TrimSpace(rawProvider))
		if provider == "" {
			continue
		}
		normalized := ReasoningProviderPolicy{Default: strings.ToLower(strings.TrimSpace(policy.Default))}
		if normalized.Default != "" && !validReasoningValue(normalized.Default) {
			return nil, fmt.Errorf("reasoning-policy.%s.default: unsupported value %q", provider, policy.Default)
		}
		for rawModel, rawValue := range policy.Models {
			model := strings.ToLower(strings.TrimSpace(rawModel))
			value := strings.ToLower(strings.TrimSpace(rawValue))
			if model == "" || value == "" {
				continue
			}
			if !validReasoningValue(value) {
				return nil, fmt.Errorf("reasoning-policy.%s.models.%s: unsupported value %q", provider, model, rawValue)
			}
			if normalized.Models == nil {
				normalized.Models = make(map[string]string)
			}
			normalized.Models[model] = value
		}
		if normalized.Default == "" && len(normalized.Models) == 0 {
			continue
		}
		out[provider] = normalized
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func validReasoningValue(value string) bool {
	switch value {
	case "auto", "-1", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	budget, err := strconv.Atoi(value)
	return err == nil && budget >= 0
}
