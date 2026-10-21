package translator

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// InjectOpenAICompatSystemInstruction prepends an operator-configured system
// message to an OpenAI chat/completions payload when the target model is listed
// in the provider's SystemInstructionModels. It gives a dedicated model alias
// its own persona without touching sibling models served by the same provider.
//
// Matching is an exact, case-insensitive comparison against the upstream model
// name. Exact matching is deliberate: substring matching would leak the persona
// into sibling models that must stay untouched. To give a model a persona,
// register a dedicated alias and list that alias here.
//
// The operator message is placed at index 0 so it precedes any client-supplied
// system message, which keeps operator precedence intact.
//
// An empty instruction or an empty model list disables the injection entirely.
func InjectOpenAICompatSystemInstruction(modelName string, rawJSON []byte, cfg *config.OpenAICompatibility) []byte {
	if cfg == nil || cfg.SystemInstruction == "" || len(cfg.SystemInstructionModels) == 0 {
		return rawJSON
	}
	if !openAICompatModelMatchesSystemInstruction(modelName, cfg.SystemInstructionModels) {
		return rawJSON
	}
	if len(rawJSON) == 0 {
		return rawJSON
	}

	// Upstream only accepts the real model name. When the caller addressed the
	// model by alias, rewrite the payload's model field to the configured
	// upstream name so the request is not rejected.
	if upstream := openAICompatUpstreamModelName(modelName, cfg); upstream != "" {
		if updated, errSet := sjson.SetBytes(rawJSON, "model", upstream); errSet == nil {
			rawJSON = updated
		}
	}
	const path = "messages"
	injected := []byte(`{"role":"system","content":""}`)
	injected, err := sjson.SetBytes(injected, "content", cfg.SystemInstruction)
	if err != nil {
		return rawJSON
	}

	existing := gjson.GetBytes(rawJSON, path)
	if !existing.Exists() || !existing.IsArray() {
		// No messages array yet: create one holding only the operator message.
		out, errSet := sjson.SetRawBytes(rawJSON, path, []byte(`[`+string(injected)+`]`))
		if errSet != nil {
			return rawJSON
		}
		return out
	}

	// Prepend at index 0 and shift the existing messages down. gjson/sjson append
	// with "parts.-1"-style indices, so build the array explicitly to keep order.
	msgs := existing.Array()
	merged := make([]byte, 0, len(injected)+len(rawJSON))
	merged = append(merged, '[')
	merged = append(merged, injected...)
	for _, m := range msgs {
		merged = append(merged, ',')
		merged = append(merged, []byte(m.Raw)...)
	}
	merged = append(merged, ']')

	out, errSet := sjson.SetRawBytes(rawJSON, path, merged)
	if errSet != nil {
		return rawJSON
	}
	return out
}

// openAICompatModelMatchesSystemInstruction reports whether modelName exactly
// equals one of the configured patterns, ignoring case and surrounding
// whitespace. A provider prefix on the requested model ("provider/model") is
// stripped so an operator can list either the bare alias or the fully
// qualified form and both resolve to the same model.
func openAICompatModelMatchesSystemInstruction(modelName string, patterns []string) bool {
	needle := normalizeCompatModelName(modelName)
	if needle == "" {
		return false
	}
	for _, pattern := range patterns {
		if p := normalizeCompatModelName(pattern); p != "" && p == needle {
			return true
		}
	}
	return false
}

// normalizeCompatModelName lowercases a model name, strips surrounding
// whitespace, and drops any provider prefix so "opencode-jb/deepseek-v4.1-flash-jb"
// and "deepseek-v4.1-flash-jb" normalize to the same value.
func normalizeCompatModelName(name string) string {
	needle := strings.ToLower(strings.TrimSpace(name))
	if idx := strings.LastIndex(needle, "/"); idx >= 0 {
		needle = needle[idx+1:]
	}
	return strings.TrimSpace(needle)
}

// openAICompatModelMatchesSystemInstructionOrUpstream reports whether the
// requested model matches a configured pattern either directly or through its
// configured upstream name. The direct match is checked first because it is the
// common case; the upstream lookup only runs when the direct match fails.
//
// This matters for callers that address a model by its upstream name rather
// openAICompatModelMatchesSystemInstructionOrUpstream reports whether the
// requested model matches a configured pattern either directly or through its
// configured alias.
//
// The alias lookup is deliberately one-directional: a request addressed by
// alias resolves through openAICompatUpstreamModelName to check whether the
// alias is listed. A request addressed by upstream name is NOT resolved back to
// an alias, because two aliases can share one upstream name — exactly the
// situation on a provider that serves both a plain and a jailbroken variant of
// the same model. Resolving in that direction would frame the plain variant
// too, which is the persona leak this whole design exists to prevent.
func openAICompatModelMatchesSystemInstructionOrUpstream(modelName string, cfg *config.OpenAICompatibility) bool {
	if cfg == nil {
		return false
	}
	if openAICompatModelMatchesSystemInstruction(modelName, cfg.SystemInstructionModels) {
		return true
	}
	// Only resolve when the requested name is itself a configured alias, so a
	// plain upstream name never inherits a sibling alias's configuration.
	if openAICompatUpstreamModelName(modelName, cfg) == "" {
		return false
	}
	return openAICompatModelMatchesSystemInstruction(modelName, cfg.SystemInstructionModels)
}

// openAICompatUpstreamModelName returns the configured upstream model name for
// a client-facing alias, or "" when the name is already the upstream name or no
// matching entry exists.
func openAICompatUpstreamModelName(modelName string, cfg *config.OpenAICompatibility) string {
	needle := normalizeCompatModelName(modelName)
	if needle == "" || cfg == nil {
		return ""
	}
	for _, m := range cfg.Models {
		if alias := normalizeCompatModelName(m.Alias); alias != "" && alias == needle {
			return strings.TrimSpace(m.Name)
		}
	}
	return ""
}

