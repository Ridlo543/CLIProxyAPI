package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReasoningPolicyAcceptsAnUnquotedBudget(t *testing.T) {
	// Operators write budgets by hand as plain numbers.
	var cfg Config
	raw := "reasoning-policy:\n  antigravity:\n    default: 12000\n    models:\n      claude-opus-4-6-thinking: 8000\n"
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	got, err := NormalizeReasoningPolicy(cfg.ReasoningPolicy)
	if err != nil {
		t.Fatalf("NormalizeReasoningPolicy() error = %v", err)
	}
	if got["antigravity"].Default != "12000" || got["antigravity"].Models["claude-opus-4-6-thinking"] != "8000" {
		t.Fatalf("policy = %#v", got)
	}
}

func TestNormalizeReasoningPolicyLowercasesAndDropsEmpty(t *testing.T) {
	got, err := NormalizeReasoningPolicy(map[string]ReasoningProviderPolicy{
		" Antigravity ": {Default: " HIGH ", Models: map[string]string{"Claude-Opus-4-6-Thinking": "8000", "blank": " "}},
		"codex":         {},
	})
	if err != nil {
		t.Fatalf("NormalizeReasoningPolicy() error = %v", err)
	}
	policy, ok := got["antigravity"]
	if !ok || policy.Default != "high" || policy.Models["claude-opus-4-6-thinking"] != "8000" {
		t.Fatalf("normalized = %#v", got)
	}
	if _, ok := policy.Models["blank"]; ok {
		t.Fatalf("empty model value kept: %#v", policy.Models)
	}
	if _, ok := got["codex"]; ok {
		t.Fatalf("provider with no settings kept: %#v", got)
	}
}

func TestNormalizeReasoningPolicyRejectsWhatThinkingCannotParse(t *testing.T) {
	for _, value := range []string{"hihg", "-5", "1.5"} {
		if _, err := NormalizeReasoningPolicy(map[string]ReasoningProviderPolicy{"antigravity": {Default: value}}); err == nil {
			t.Errorf("default %q accepted, want an error", value)
		}
	}
	for _, value := range []string{"auto", "-1", "none", "minimal", "max", "0", "32000"} {
		if _, err := NormalizeReasoningPolicy(map[string]ReasoningProviderPolicy{"antigravity": {Default: value}}); err != nil {
			t.Errorf("default %q rejected: %v", value, err)
		}
	}
}
