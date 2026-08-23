// Package apikeypolicy enforces per-client-API-key restrictions defined in
// the additive `api-key-policies` config section.
//
// Known limitation: token usage counters are in-memory only and reset on
// process restart. "total" windows therefore count since process start.
package apikeypolicy

import (
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type windowState struct {
	limit       int64
	window      string
	windowStart time.Time
	firstSeen   time.Time
	used        int64
}

// Enforcer holds policies plus in-memory per-key usage counters.
type Enforcer struct {
	mu       sync.RWMutex
	byKey    map[string]config.APIKeyPolicy
	usage    map[string]*windowState
	nowClock func() time.Time
}

var defaultEnforcer = &Enforcer{nowClock: time.Now}

// Default returns the process-wide enforcer singleton.
func Default() *Enforcer { return defaultEnforcer }

// SetClock overrides the clock; intended for tests.
func (e *Enforcer) SetClock(now func() time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if now == nil {
		e.nowClock = time.Now
	} else {
		e.nowClock = now
	}
}

// Replace atomically swaps the active policy set.
func (e *Enforcer) Replace(policies []config.APIKeyPolicy) {
	byKey := make(map[string]config.APIKeyPolicy, len(policies))
	for _, p := range policies {
		byKey[p.Key] = p
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.byKey = byKey
}
func (e *Enforcer) policy(key string) (config.APIKeyPolicy, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	p, ok := e.byKey[key]
	return p, ok
}

// Policy returns the configured policy for a key, if any.
func (e *Enforcer) Policy(key string) (config.APIKeyPolicy, bool) { return e.policy(key) }

// CheckModel enforces exact-match model allowlists.
func (e *Enforcer) CheckModel(key, model string) bool {
	p, ok := e.policy(key)
	if !ok || len(p.Models) == 0 {
		return true
	}
	model = strings.TrimSpace(model)
	for _, allowed := range p.Models {
		if allowed == model {
			return true
		}
	}
	return false
}

// CheckProviders denies requests whose model cannot be served by any allowed
// provider. candidates are the providers known to serve this model; unknown
// models yield no candidates and pass only when the allowlist itself is empty.
func (e *Enforcer) CheckProviders(key string, candidates []string, modelKnown bool) bool {
	p, ok := e.policy(key)
	if !ok || len(p.Providers) == 0 {
		return true
	}
	if !modelKnown || len(candidates) == 0 {
		return false
	}
	allowed := make(map[string]struct{}, len(p.Providers))
	for _, provider := range p.Providers {
		allowed[strings.ToLower(strings.TrimSpace(provider))] = struct{}{}
	}
	for _, candidate := range candidates {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(candidate))]; ok {
			return true
		}
	}
	return false
}

// CheckBudget reports whether the key still has token budget remaining.
func (e *Enforcer) CheckBudget(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	state, ok := e.usage[key]
	if !ok || state.limit <= 0 {
		return true
	}
	e.rollWindowLocked(key, state)
	return state.used < state.limit
}

// Record adds consumed total tokens for a client key.
func (e *Enforcer) Record(key string, totalTokens int64) {
	if strings.TrimSpace(key) == "" || totalTokens <= 0 {
		return
	}
	p, ok := e.policy(key)
	if !ok || p.Limit == nil || p.Limit.Limit <= 0 {
		// Track even unlimited keys so the usage endpoint can report activity.
		e.mu.Lock()
		state := e.usageStateLocked(key, "", 0)
		state.used += totalTokens
		e.mu.Unlock()
		return
	}
	e.mu.Lock()
	state := e.usageStateLocked(key, p.Limit.Window, p.Limit.Limit)
	e.rollWindowLocked(key, state)
	state.used += totalTokens
	e.mu.Unlock()
}

func (e *Enforcer) usageStateLocked(key, window string, limit int64) *windowState {
	if e.usage == nil {
		e.usage = map[string]*windowState{}
	}
	state, ok := e.usage[key]
	if !ok {
		now := e.nowClock().UTC()
		state = &windowState{window: window, limit: limit, windowStart: now, firstSeen: now}
		e.usage[key] = state
		return state
	}
	state.window, state.limit = window, limit
	return state
}

// rollWindowLocked resets counters at UTC day/month boundaries. Caller holds mu.
func (e *Enforcer) rollWindowLocked(key string, state *windowState) {
	now := e.nowClock().UTC()
	reset := false
	switch state.window {
	case config.TokenWindowDaily:
		y1, m1, d1 := state.windowStart.Date()
		y2, m2, d2 := now.Date()
		reset = y1 != y2 || m1 != m2 || d1 != d2
	case config.TokenWindowMonthly:
		reset = state.windowStart.Year() != now.Year() || state.windowStart.Month() != now.Month()
	case config.TokenWindowTotal:
		// Never resets within the process lifetime.
	default:
		return
	}
	if reset {
		state.used = 0
		state.windowStart = now
	}
}

// WindowUsage is a sanitized per-key accounting snapshot.
type WindowUsage struct {
	KeyMasked   string    `json:"key"`
	Window      string    `json:"window"`
	Limit       int64     `json:"limit"`
	WindowStart time.Time `json:"window_start"`
	TokensUsed  int64     `json:"tokens_used"`
}

// MaskedKey renders a key as its last four characters.
func MaskedKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "…" + key[len(key)-4:]
}

// UsageSnapshot returns current counters with masked keys.
func (e *Enforcer) UsageSnapshot() []WindowUsage {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]WindowUsage, 0, len(e.usage))
	for key, state := range e.usage {
		e.rollWindowLocked(key, state)
		out = append(out, WindowUsage{
			KeyMasked:   MaskedKey(key),
			Window:      state.window,
			Limit:       state.limit,
			WindowStart: state.windowStart,
			TokensUsed:  state.used,
		})
	}
	return out
}
