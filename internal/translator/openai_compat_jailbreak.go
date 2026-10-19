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
// whitespace.
func openAICompatModelMatchesSystemInstruction(modelName string, patterns []string) bool {
	needle := strings.ToLower(strings.TrimSpace(modelName))
	if needle == "" {
		return false
	}
	for _, pattern := range patterns {
		if strings.ToLower(strings.TrimSpace(pattern)) == needle {
			return true
		}
	}
	return false
}
