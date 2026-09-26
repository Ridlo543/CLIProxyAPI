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

	"github.com/router-for-me/CLIProxyAPI/v7/internal/combos"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type rateMinuteState struct {
	minuteStart time.Time
	requests    int
	tokens      int64
}

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
	rateMap  map[string]*rateMinuteState
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

// CheckModel enforces model allowlists with support for provider-namespaced IDs.
func (e *Enforcer) CheckModel(key, model string) bool {
	p, ok := e.policy(key)
	if !ok || len(p.Models) == 0 {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return true
	}
	reqProv, reqBare, reqHasSlash := strings.Cut(model, "/")

	for _, allowed := range p.Models {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}
		// 1. Exact match (case-insensitive)
		if strings.EqualFold(allowed, model) {
			return true
		}
		allowProv, allowBare, allowHasSlash := strings.Cut(allowed, "/")
		if allowHasSlash && reqHasSlash {
			// Both are qualified: "openagentic/gemini" vs "openagentic/gemini"
			if strings.EqualFold(allowProv, reqProv) && strings.EqualFold(allowBare, reqBare) {
				return true
			}
		} else if !allowHasSlash && reqHasSlash {
			// Allowed is bare "gemini", request is "openagentic/gemini"
			if strings.EqualFold(allowed, reqBare) {
				return true
			}
		} else if allowHasSlash && !reqHasSlash {
			// Allowed is "openagentic/gemini", request is bare "gemini"
			if strings.EqualFold(allowBare, model) {
				return true
			}
		}
	}
	return false
}

// PinnedProviderForModel returns the provider prefix if the key's policy specifically
// allowed this model under a specific provider namespace (e.g. "openagentic/gemini-2.5-flash")
// while the client requested bare "gemini-2.5-flash".
func (e *Enforcer) PinnedProviderForModel(key, model string) string {
	p, ok := e.policy(key)
	if !ok || len(p.Models) == 0 {
		return ""
	}
	model = strings.TrimSpace(model)
	if strings.Contains(model, "/") {
		return ""
	}
	for _, allowed := range p.Models {
		allowProv, allowBare, allowHasSlash := strings.Cut(strings.TrimSpace(allowed), "/")
		if allowHasSlash && strings.EqualFold(allowBare, model) {
			return allowProv
		}
	}
	return ""
}

// IsModelAllowed reports whether a model (and its owner provider) is visible and callable
// by the given API key. Used for /v1/models listing filtering.
func (e *Enforcer) IsModelAllowed(key, modelID, ownedBy string) bool {
	p, ok := e.policy(key)
	if !ok || (len(p.Models) == 0 && len(p.Providers) == 0) {
		return true
	}
	modelID = strings.TrimSpace(modelID)
	ownedBy = strings.TrimSpace(ownedBy)
	ownedBy = strings.TrimPrefix(ownedBy, "openai-compatible-")

	reqProv, reqBare, reqHasSlash := strings.Cut(modelID, "/")
	if !reqHasSlash {
		reqBare = modelID
		reqProv = ownedBy
	}

	// 1. Provider restriction check
	if len(p.Providers) > 0 {
		allowedProvMap := make(map[string]struct{}, len(p.Providers))
		for _, prov := range p.Providers {
			allowedProvMap[strings.ToLower(strings.TrimSpace(prov))] = struct{}{}
		}

		providerMatches := false
		if ownedBy == "combos" || reqProv == "combos" {
			if cmb, ok := combos.Find(reqBare); ok {
				for _, member := range cmb.Models {
					if _, okP := allowedProvMap[strings.ToLower(strings.TrimSpace(member.Provider))]; okP {
						providerMatches = true
						break
					}
				}
			}
		} else {
			for _, check := range []string{reqProv, ownedBy} {
				check = strings.ToLower(strings.TrimSpace(check))
				if check == "" {
					continue
				}
				if _, okP := allowedProvMap[check]; okP {
					providerMatches = true
					break
				}
			}
		}
		if !providerMatches {
			return false
		}
	}

	// 2. Model restriction check
	if len(p.Models) > 0 {
		modelMatches := false
		for _, allowed := range p.Models {
			allowed = strings.TrimSpace(allowed)
			if allowed == "" {
				continue
			}
			if strings.EqualFold(allowed, modelID) {
				modelMatches = true
				break
			}
			allowProv, allowBare, allowHasSlash := strings.Cut(allowed, "/")
			if allowHasSlash && reqHasSlash {
				if strings.EqualFold(allowProv, reqProv) && strings.EqualFold(allowBare, reqBare) {
					modelMatches = true
					break
				}
			} else if allowHasSlash && !reqHasSlash {
				// Policy has "openagentic/gemini", model is bare "gemini".
				// Matches only if this bare model belongs to openagentic.
				if strings.EqualFold(allowBare, modelID) && (strings.EqualFold(allowProv, ownedBy) || strings.EqualFold(allowProv, reqProv)) {
					modelMatches = true
					break
				}
			} else if !allowHasSlash && reqHasSlash {
				// Policy has bare "gemini", model in list is "openagentic/gemini".
				if strings.EqualFold(allowed, reqBare) {
					modelMatches = true
					break
				}
			}
		}
		if !modelMatches {
			return false
		}
	}

	return true
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
		cand := strings.ToLower(strings.TrimSpace(candidate))
		candClean := strings.TrimPrefix(cand, "openai-compatible-")
		if _, ok := allowed[cand]; ok {
			return true
		}
		if _, ok := allowed[candClean]; ok {
			return true
		}
		for allow := range allowed {
			allowClean := strings.TrimPrefix(allow, "openai-compatible-")
			if allowClean == candClean {
				return true
			}
			if strings.HasPrefix(allowClean, candClean+"-") || strings.HasPrefix(candClean, allowClean+"-") {
				return true
			}
		}
	}
	return false
}

// CheckRateLimit checks both RPM and TPM thresholds. Returns false if rate limit is exceeded.
func (e *Enforcer) CheckRateLimit(key string) bool {
	p, ok := e.policy(key)
	if !ok || (p.RPM <= 0 && p.TPM <= 0) {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rateMap == nil {
		e.rateMap = make(map[string]*rateMinuteState)
	}
	now := e.nowClock()
	st, exists := e.rateMap[key]
	if !exists || now.Sub(st.minuteStart) >= time.Minute {
		st = &rateMinuteState{minuteStart: now}
		e.rateMap[key] = st
	}
	if p.RPM > 0 && st.requests >= p.RPM {
		return false
	}
	if p.TPM > 0 && st.tokens >= p.TPM {
		return false
	}
	st.requests++
	return true
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rateMap != nil {
		if st, ok := e.rateMap[key]; ok {
			st.tokens += totalTokens
		}
	}
	p, ok := e.byKey[key]
	if !ok || p.Limit == nil || p.Limit.Limit <= 0 {
		state := e.usageStateLocked(key, "", 0)
		state.used += totalTokens
		return
	}
	state := e.usageStateLocked(key, p.Limit.Window, p.Limit.Limit)
	e.rollWindowLocked(key, state)
	state.used += totalTokens
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
