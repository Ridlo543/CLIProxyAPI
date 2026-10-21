// Package combos is the runtime side of the model-combination feature
// (config types live in internal/config/config_combos.go).
//
// Isolation note: everything the request pipeline needs from this package is
// a tiny read-only snapshot store plus pure helpers — no executor, auth, or
// translator imports — so pulling CLIProxyAPI upstream stays conflict-free.
package combos

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// Providers that opted out of cooling keep their legacy always-try behaviour.
	policy := DefaultPolicy()
	policy.Disabled = cfg.DisableCooling
	var fp strings.Builder
	if cfg.DisableCooling {
		fp.WriteString("global-disable;")
	}
	for _, compat := range cfg.OpenAICompatibility {
		if compat.DisableCooling != nil && *compat.DisableCooling {
			if policy.DisabledProviders == nil {
				policy.DisabledProviders = map[string]struct{}{}
			}
			name := strings.ToLower(strings.TrimSpace(compat.Name))
			policy.DisabledProviders[name] = struct{}{}
			fp.WriteString("off:" + name + ";")
		}
	}
	for _, c := range list {
		fp.WriteString(strings.ToLower(c.Name))
		for _, m := range c.Models {
			fp.WriteString("|" + MemberKey(m.Provider, m.Model))
		}
		fp.WriteString(";")
	}
	SyncHealthPolicy(policy, fp.String())
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
	sticky := c.Sticky
	if sticky <= 0 {
		sticky = 1
	}
	key := strings.ToLower(c.Name)
	rawState, _ := rrIndex.LoadOrStore(key, &rotationState{})
	state := rawState.(*rotationState)

	state.mu.Lock()
	head := state.index % len(members)
	rotated := append(append([]config.ComboModelRef(nil), members[head:]...), members[:head]...)
	state.used++
	if state.used >= sticky {
		state.index = (head + 1) % len(members)
		state.used = 0
	}
	state.mu.Unlock()
	return rotated
}

// rotationState is the per-combo round-robin cursor. Sticky counts consecutive
// requests served by one member before the cursor advances, which keeps
// provider-side prefix caches warm.
type rotationState struct {
	mu    sync.Mutex
	index int
	used  int
}

// ShouldFallbackStatus mirrors AinyRouter's accountFallback rules: transient,
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

// contextExceededMarkers identify upstream rejections caused by a prompt that is
// larger than the member's context window. They must fall through to a member
// with a larger window instead of being returned to the client.
var contextExceededMarkers = []string{
	"context_length_exceeded",
	"maximum context length",
	"context length exceeded",
	"exceeds the maximum",
	"prompt is too long",
	"input is too long",
	"too many tokens",
	"max_tokens_exceeded",
}

var verificationMarkers = []string{
	"verify your account",
	"verification required",
	"verification_required",
	"account verification",
}

var modelMissingMarkers = []string{
	"model_not_found",
	"model not found",
	"invalid model",
	"unknown model",
	"does not exist",
	"not supported",
	"unsupported model",
}

// IsContextExceeded reports whether the upstream rejected the request because it
// did not fit the member's context window.
func IsContextExceeded(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge && status != http.StatusUnprocessableEntity {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, marker := range contextExceededMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// IsVerificationRequired reports whether the upstream asked the account owner to
// verify the account. These recover on the order of hours, never seconds.
func IsVerificationRequired(status int, body []byte) bool {
	if status < http.StatusBadRequest {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, marker := range verificationMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// IsModelMissing reports whether the member's model is unknown to the upstream.
func IsModelMissing(status int, body []byte) bool {
	if status != http.StatusNotFound && status != http.StatusBadRequest {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, marker := range modelMissingMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// IsPoisonRequest reports a client-side fault that no other member can fix:
// retrying it only multiplies latency and consumes quota. Poison requests are
// never cooled and never fall through.
func IsPoisonRequest(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	if IsContextExceeded(status, body) {
		return false // another member with a bigger window may accept it
	}
	return !ShouldFallback(status, body)
}

// ShouldFallback reports whether a failure (status code + response body)
// warrants trying the next candidate in a combo.
func ShouldFallback(status int, body []byte) bool {
	if ShouldFallbackStatus(status) {
		return true
	}
	if IsContextExceeded(status, body) {
		return true
	}
	if IsModelMissing(status, body) {
		return true
	}
	if status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		if len(body) > 0 {
			// Match credential/quota phrases, not bare words: "rate" also matches
			// "temperature" and "token" matches "max_tokens", which turned genuine
			// client mistakes into needless member attempts.
			lower := strings.ToLower(string(body))
			for _, marker := range fallbackBodyMarkers {
				if strings.Contains(lower, marker) {
					return true
				}
			}
		}
	}
	return false
}

// fallbackBodyMarkers are credential/quota/session signals that make a 400/404/422
// worth retrying on another member.
var fallbackBodyMarkers = []string{
	"quota",
	"capacity",
	"no_biscuit",
	"biscuit",
	"overloaded",
	"rate limit",
	"rate_limit",
	"ratelimit",
	"rate-limit",
	"too many requests",
	"invalid token",
	"invalid api key",
	"session expired",
	"token expired",
	"token limit",
	"credential",
	"unauthorized",
}

// ExtractRetryAfter reads the retry hint an upstream sent with a failure.
// It understands the Retry-After header, "retry-after-ms"/"retry_after" JSON
// fields, and Gemini-style RetryInfo details. Unparseable hints yield zero.
func ExtractRetryAfter(status int, header string, body []byte) time.Duration {
	if status < http.StatusBadRequest {
		return 0
	}
	if d := parseRetryAfterHeader(header); d > 0 {
		return d
	}
	if len(body) == 0 {
		return 0
	}
	for _, field := range []string{"retry-after-ms", "retry_after_ms", "retry-after", "retry_after"} {
		if v := gjson.GetBytes(body, field); v.Exists() {
			if d := retryAfterFromValue(v.String(), strings.HasSuffix(field, "-ms") || strings.HasSuffix(field, "_ms")); d > 0 {
				return d
			}
		}
	}
	// Nested shapes: {"error":{"retry_after":...}} and Gemini RetryInfo.
	for _, field := range []string{"error.retry-after-ms", "error.retry-after", "error.details"} {
		if v := gjson.GetBytes(body, field); v.Exists() {
			if field == "error.details" && v.IsArray() {
				for _, item := range v.Array() {
					if strings.Contains(strings.ToLower(item.Get("type").String()), "retryinfo") {
						if d := parseRetryAfterHeader(item.Get("retryDelay").String()); d > 0 {
							return d
						}
					}
				}
				continue
			}
			if d := retryAfterFromValue(v.String(), strings.Contains(field, "ms")); d > 0 {
				return d
			}
		}
	}
	return 0
}

func retryAfterFromValue(raw string, milliseconds bool) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		if milliseconds || f > 3600 {
			return time.Duration(f) * time.Millisecond
		}
		return time.Duration(f * float64(time.Second))
	}
	return parseRetryAfterHeader(raw)
}

func parseRetryAfterHeader(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		if f <= 0 {
			return 0
		}
		return time.Duration(f * float64(time.Second))
	}
	if when, err := http.ParseTime(raw); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// ModelID renders a member reference in "provider/model" form for logs.
func ModelID(m config.ComboModelRef) string { return m.Provider + "/" + m.Model }
