package config

import (
	"fmt"
	"strings"
)

// Supported per-API-key token budget windows.
const (
	TokenWindowNone    = ""
	TokenWindowDaily   = "daily"
	TokenWindowMonthly = "monthly"
	TokenWindowTotal   = "total"
)

// APIKeyTokenLimit bounds the tokens one client API key may consume.
type APIKeyTokenLimit struct {
	Window string `yaml:"window" json:"window"`
	Limit  int64  `yaml:"limit" json:"limit"`
}

// APIKeyPolicy attaches additive restrictions to one exact client key.
type APIKeyPolicy struct {
	Key       string            `yaml:"key,omitempty" json:"key,omitempty"`
	APIKey    string            `yaml:"api-key,omitempty" json:"api-key,omitempty"`
	Name      string            `yaml:"name,omitempty" json:"name,omitempty"`
	Models    []string          `yaml:"models,omitempty" json:"models,omitempty"`
	Providers []string          `yaml:"providers,omitempty" json:"providers,omitempty"`
	Limit     *APIKeyTokenLimit `yaml:"token-limit,omitempty" json:"token-limit,omitempty"`
}

// EffectiveKey returns the key string from either Key or APIKey.
func (p APIKeyPolicy) EffectiveKey() string {
	if strings.TrimSpace(p.Key) != "" {
		return strings.TrimSpace(p.Key)
	}
	return strings.TrimSpace(p.APIKey)
}

// NormalizeAPIKeyPolicies trims and validates the api-key-policies section.
func NormalizeAPIKeyPolicies(policies []APIKeyPolicy) ([]APIKeyPolicy, error) {
	seen := make(map[string]struct{}, len(policies))
	out := make([]APIKeyPolicy, 0, len(policies))
	for i := range policies {
		p := policies[i]
		effKey := p.EffectiveKey()
		if effKey == "" {
			return nil, fmt.Errorf("api-key-policies[%d]: key must not be empty", i)
		}
		p.Key = effKey
		p.Name = strings.TrimSpace(p.Name)
		if _, dup := seen[p.Key]; dup {
			return nil, fmt.Errorf("api-key-policies[%d]: duplicate key", i)
		}
		seen[p.Key] = struct{}{}
		p.Models = normalizeStringList(p.Models)
		p.Providers = normalizeStringList(p.Providers)
		if p.Limit != nil {
			window := strings.ToLower(strings.TrimSpace(p.Limit.Window))
			switch window {
			case TokenWindowNone, TokenWindowDaily, TokenWindowMonthly, TokenWindowTotal:
				p.Limit.Window = window
			default:
				return nil, fmt.Errorf("api-key-policies[%d]: token-limit.window must be empty, daily, monthly, or total", i)
			}
			if p.Limit.Limit < 0 {
				return nil, fmt.Errorf("api-key-policies[%d]: token-limit.limit must not be negative", i)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
