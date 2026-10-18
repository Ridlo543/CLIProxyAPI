package gemini

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// InjectSystemInstruction prepends an operator-configured system instruction to
// the request's systemInstruction when the target model is listed in the
// configuration. The operator text is placed first so the client's own system
// instruction (if any) follows it, which keeps upstream precedence rules intact.
//
// Matching is an exact, case-insensitive comparison against the upstream model
// name. Exact matching is deliberate: substring matching would leak the persona
// into sibling models such as gemini-3.8-flash-high and gemini-3.8-flash-tiered,
// which are distinct upstream models that must stay untouched. To give a model a
// persona, register a dedicated alias and list that alias here.
//
// An empty instruction or an empty model list disables the injection entirely.
func InjectSystemInstruction(modelName string, rawJSON []byte, cfg *config.AntigravityConfig) []byte {
	if cfg == nil || cfg.SystemInstruction == "" || len(cfg.SystemInstructionModels) == 0 {
		return rawJSON
	}
	if !modelMatchesSystemInstruction(modelName, cfg.SystemInstructionModels) {
		return rawJSON
	}

	const path = "request.systemInstruction"
	injected := []byte(`{"role":"user","parts":[{"text":""}]}`)
	injected, _ = sjson.SetBytes(injected, "parts.0.text", cfg.SystemInstruction)

	existing := gjson.GetBytes(rawJSON, path)
	if !existing.Exists() {
		out, err := sjson.SetRawBytes(rawJSON, path, injected)
		if err != nil {
			return rawJSON
		}
		return out
	}

	existingParts := existing.Get("parts")
	if existingParts.IsArray() && len(existingParts.Array()) > 0 {
		merged := []byte(`{"role":"user","parts":[]}`)
		// Operator instruction first so it takes precedence, client parts after.
		merged, _ = sjson.SetRawBytes(merged, "parts.0", injectedPart(injected))
		for _, part := range existingParts.Array() {
			merged, _ = sjson.SetRawBytes(merged, "parts.-1", []byte(part.Raw))
		}
		out, err := sjson.SetRawBytes(rawJSON, path, merged)
		if err != nil {
			return rawJSON
		}
		return out
	}

	out, err := sjson.SetRawBytes(rawJSON, path, injected)
	if err != nil {
		return rawJSON
	}
	return out
}

// injectedPart returns the first part of a systemInstruction node.
func injectedPart(node []byte) []byte {
	part := gjson.GetBytes(node, "parts.0")
	if part.Exists() {
		return []byte(part.Raw)
	}
	return []byte(`{"text":""}`)
}

// modelMatchesSystemInstruction reports whether modelName exactly equals one of
// the configured patterns, ignoring case and surrounding whitespace.
func modelMatchesSystemInstruction(modelName string, patterns []string) bool {
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
