package thinking

import (
	"strings"
	"sync/atomic"
)

// ProviderPolicy is the operator-forced thinking setting for one provider.
// Values use the model-suffix grammar (auto, none, a level, or a budget).
type ProviderPolicy struct {
	Default string
	Models  map[string]string
}

var reasoningPolicy atomic.Pointer[map[string]ProviderPolicy]

// SetReasoningPolicy replaces the forced reasoning settings. Keys are provider
// identifiers as executors report them; model keys are upstream model IDs.
// Passing nil clears every policy.
func SetReasoningPolicy(policies map[string]ProviderPolicy) {
	if len(policies) == 0 {
		reasoningPolicy.Store(nil)
		return
	}
	copied := make(map[string]ProviderPolicy, len(policies))
	for provider, policy := range policies {
		models := make(map[string]string, len(policy.Models))
		for model, value := range policy.Models {
			models[strings.ToLower(strings.TrimSpace(model))] = strings.TrimSpace(value)
		}
		copied[strings.ToLower(strings.TrimSpace(provider))] = ProviderPolicy{Default: strings.TrimSpace(policy.Default), Models: models}
	}
	reasoningPolicy.Store(&copied)
}

// forcedReasoningConfig returns the policy that overrides the request for this
// provider and model: the model entry first, then the provider default.
func forcedReasoningConfig(providerKey, model string) (ThinkingConfig, bool) {
	policies := reasoningPolicy.Load()
	if policies == nil {
		return ThinkingConfig{}, false
	}
	policy, ok := (*policies)[strings.ToLower(strings.TrimSpace(providerKey))]
	if !ok {
		return ThinkingConfig{}, false
	}
	raw := policy.Models[strings.ToLower(strings.TrimSpace(model))]
	if raw == "" {
		raw = policy.Default
	}
	if raw == "" {
		return ThinkingConfig{}, false
	}
	config := parseSuffixToConfig(raw, providerKey, model)
	return config, hasThinkingConfig(config)
}
