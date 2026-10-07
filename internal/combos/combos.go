// Package combos is the runtime side of the model-combination feature
// (config types live in internal/config/config_combos.go).
//
// Isolation note: everything the request pipeline needs from this package is
// a tiny read-only snapshot store plus pure helpers — no executor, auth, or
// translator imports — so pulling CLIProxyAPI upstream stays conflict-free.
package combos

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

var (
	mu              sync.RWMutex
	snapshot        []config.ComboConfig
	capacityAdapter config.CapacityAdapterConfig
	// rrIndex tracks per-combo rotation state for round-robin strategy.
	rrIndex sync.Map // combo name(lowercased) -> *atomic.Uint64
	// adapterRR tracks capacity adapter rotation
	adapterRR atomic.Uint64
)

// SyncFromConfig replaces the in-memory snapshot. Called by
// pluginhost.Host.ApplyConfig so boot AND every management save stay in sync.
func SyncFromConfig(cfg *config.Config) {
	list := make([]config.ComboConfig, 0, len(cfg.Combos))
	for _, c := range cfg.Combos {
		c.Normalize()
		list = append(list, c.Clone())
	}
	adapter := cfg.CapacityAdapter
	adapter.Normalize()

	mu.Lock()
	snapshot = list
	capacityAdapter = adapter.Clone()
	mu.Unlock()
}

func SnapshotCapacityAdapter() config.CapacityAdapterConfig {
	mu.RLock()
	defer mu.RUnlock()
	return capacityAdapter.Clone()
}

// GetVisionAdapterModels returns the ordered models to route vision requests to.
func GetVisionAdapterModels() []config.ComboModelRef {
	mu.RLock()
	capCfg := capacityAdapter.Vision
	mu.RUnlock()
	if !capCfg.Enabled || len(capCfg.Models) == 0 {
		return nil
	}
	models := append([]config.ComboModelRef(nil), capCfg.Models...)
	if len(models) > 1 && capCfg.RoundRobin {
		idx := int(adapterRR.Add(1)-1) % len(models)
		return append(append([]config.ComboModelRef(nil), models[idx:]...), models[:idx]...)
	}
	return models
}

// HasVisionCapability reports if a given model natively supports image understanding.
func HasVisionCapability(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if _, after, ok := strings.Cut(m, "/"); ok {
		m = after
	}
	// Models that definitely do not support images (embeddings, audio, review, moderation)
	if strings.Contains(m, "-review") ||
		strings.Contains(m, "embed") ||
		strings.Contains(m, "rerank") ||
		strings.Contains(m, "whisper") ||
		strings.Contains(m, "tts") ||
		strings.Contains(m, "moderation") {
		return false
	}
	// Models known to support vision across modern foundation model families
	if strings.Contains(m, "flash") ||
		strings.Contains(m, "pro") ||
		strings.Contains(m, "gemini") ||
		strings.Contains(m, "claude") ||
		strings.Contains(m, "gpt") ||
		strings.Contains(m, "sol") ||
		strings.Contains(m, "luna") ||
		strings.Contains(m, "astra") ||
		strings.Contains(m, "terra") ||
		strings.Contains(m, "kimi") ||
		strings.Contains(m, "minimax") ||
		strings.Contains(m, "vision") ||
		strings.Contains(m, "vl") ||
		strings.Contains(m, "qwen") ||
		strings.Contains(m, "glm") ||
		strings.Contains(m, "grok") ||
		strings.Contains(m, "deepseek") ||
		strings.Contains(m, "mimo") ||
		strings.Contains(m, "llama") ||
		strings.Contains(m, "mistral") ||
		strings.Contains(m, "fable") {
		return true
	}
	return false
}

// RequestRequiresVision inspects request payload structurally for real image blocks.
// It checks message parts structurally rather than doing raw substring matching on prompt text,
// preventing false positives when prompts discuss code, MIME types, or HTML tags.
func RequestRequiresVision(rawJSON []byte) bool {
	if len(rawJSON) == 0 {
		return false
	}

	// 1. OpenAI & Anthropic: check messages array
	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		for _, msg := range messages.Array() {
			content := msg.Get("content")
			if content.IsArray() {
				for _, part := range content.Array() {
					partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
					if partType == "image_url" || partType == "image" || partType == "input_image" {
						return true
					}
				}
			}
		}
	}

	// 2. Google Gemini: check contents array
	contents := gjson.GetBytes(rawJSON, "contents")
	if contents.IsArray() {
		for _, c := range contents.Array() {
			parts := c.Get("parts")
			if parts.IsArray() {
				for _, part := range parts.Array() {
					if part.Get("inline_data").Exists() || part.Get("inlineData").Exists() {
						return true
					}
					if fileData := part.Get("file_data"); fileData.Exists() {
						if strings.HasPrefix(strings.ToLower(fileData.Get("mime_type").String()), "image/") {
							return true
						}
					}
					if fileData := part.Get("fileData"); fileData.Exists() {
						if strings.HasPrefix(strings.ToLower(fileData.Get("mimeType").String()), "image/") {
							return true
						}
					}
				}
			}
		}
	}

	// 3. Responses API / multimodal prompt
	prompt := gjson.GetBytes(rawJSON, "prompt")
	if prompt.IsArray() {
		for _, part := range prompt.Array() {
			partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
			if partType == "image_url" || partType == "image" || partType == "input_image" {
				return true
			}
		}
	}

	// 4. Input images at root (e.g. specialized multimodal endpoints)
	if gjson.GetBytes(rawJSON, "input_image").Exists() || gjson.GetBytes(rawJSON, "image").IsObject() {
		return true
	}

	return false
}

func Snapshot() []config.ComboConfig {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]config.ComboConfig, 0, len(snapshot))
	for _, c := range snapshot {
		out = append(out, c.Clone())
	}
	return out
}

// SnapshotCount reports how many combos are currently loaded.
func SnapshotCount() int {
	mu.RLock()
	defer mu.RUnlock()
	return len(snapshot)
}

// Find returns a deep copy of the combo with the given (case-insensitive)
// name, or nil.
func Find(name string) (config.ComboConfig, bool) {
	target := strings.ToLower(strings.TrimSpace(name))
	mu.RLock()
	defer mu.RUnlock()
	for _, c := range snapshot {
		if strings.ToLower(c.Name) == target {
			return c.Clone(), true
		}
	}
	return config.ComboConfig{}, false
}

// Order applies the combo's strategy to produce the attempt chain:
//   - fallback: members in listed order
//   - round-robin: rotate the head by a per-combo monotonically increasing
//     counter, then keep listed order for the tail (failures fall through).
func Order(c config.ComboConfig) []config.ComboModelRef {
	members := append([]config.ComboModelRef(nil), c.Models...)
	if len(members) < 2 || c.Strategy != config.ComboStrategyRoundRobin {
		return members
	}
	key := strings.ToLower(c.Name)
	rawIdx, _ := rrIndex.LoadOrStore(key, new(atomic.Uint64))
	head := int(rawIdx.(*atomic.Uint64).Add(1)-1) % len(members)
	return append(append([]config.ComboModelRef(nil), members[head:]...), members[:head]...)
}

// ShouldFallbackStatus mirrors 9Router's accountFallback rules: transient,
// capacity, and auth-exhaustion failures try the next member; definite client
// mistakes do not.
func ShouldFallbackStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests, // 429
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		http.StatusInsufficientStorage: // rare upstream capacity signal
		return true
	case http.StatusUnauthorized, http.StatusForbidden:
		// Other members may still be authorized with their own credentials.
		return true
	default:
		return false
	}
}

// ShouldFallback reports whether a failure (status code + response body)
// warrants trying the next candidate in a combo.
func ShouldFallback(status int, body []byte) bool {
	if ShouldFallbackStatus(status) {
		return true
	}
	if status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		if len(body) > 0 {
			lower := strings.ToLower(string(body))
			if strings.Contains(lower, "not supported") ||
				strings.Contains(lower, "unsupported") ||
				strings.Contains(lower, "does not exist") ||
				strings.Contains(lower, "not found") ||
				strings.Contains(lower, "not available") ||
				strings.Contains(lower, "invalid model") ||
				strings.Contains(lower, "unknown model") ||
				strings.Contains(lower, "model_not_found") ||
				strings.Contains(lower, "quota") ||
				strings.Contains(lower, "capacity") ||
				strings.Contains(lower, "no_biscuit") ||
				strings.Contains(lower, "biscuit") ||
				strings.Contains(lower, "token") ||
				strings.Contains(lower, "session") ||
				strings.Contains(lower, "credential") ||
				strings.Contains(lower, "overloaded") ||
				strings.Contains(lower, "rate") {
				return true
			}
		}
	}
	return false
}


// ModelID renders a member reference in "provider/model" form for logs.
func ModelID(m config.ComboModelRef) string { return m.Provider + "/" + m.Model }
