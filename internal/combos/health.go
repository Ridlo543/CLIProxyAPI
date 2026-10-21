package combos

import (
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// MemberState is the circuit-breaker state of one combo member
// (one provider/model pair).
type MemberState string

const (
	// MemberClosed means the member is healthy and eligible for every request.
	MemberClosed MemberState = "closed"
	// MemberOpen means the member failed recently and is skipped until the
	// cooldown expires.
	MemberOpen MemberState = "open"
	// MemberHalfOpen means the cooldown expired; exactly one request probes the
	// member while the rest keep skipping it.
	MemberHalfOpen MemberState = "half-open"
)

// Default cooldown values. Chosen so a failing member is skipped quickly on the
// next request while still recovering on its own: transient faults start at 30s
// and double, hard auth faults wait 2m, and account-verification faults (which
// only a human can clear) wait 30m.
const (
	DefaultTransientCooldown    = 30 * time.Second
	DefaultAuthCooldown         = 2 * time.Minute
	DefaultVerificationCooldown = 30 * time.Minute
	DefaultNotFoundCooldown     = 5 * time.Minute
	DefaultMaxCooldown          = 5 * time.Minute
	DefaultRetryAfterCap        = 30 * time.Minute
	DefaultRequestBudget        = 4 * time.Minute
	// probeLease bounds how long a half-open probe may stay unresolved before
	// another request is allowed to probe the member again.
	probeLease = 2 * time.Minute
)

// Policy is the resolved combo member circuit-breaker policy.
//
// Zero value means "all defaults" so callers can pass Policy{} in tests.
type Policy struct {
	Enabled               bool
	Disabled              bool
	TransientCooldown     time.Duration
	AuthCooldown          time.Duration
	VerificationCooldown  time.Duration
	NotFoundCooldown      time.Duration
	MaxCooldown           time.Duration
	RetryAfterCap         time.Duration
	RequestBudget         time.Duration
	BestEffortWhenAllOpen bool
	// DisabledProviders lists providers that opted out of cooling
	// (disable-cooling: true), keyed by lowercased provider name.
	DisabledProviders map[string]struct{}
}

// DefaultPolicy returns the production defaults.
func DefaultPolicy() Policy {
	return Policy{
		Enabled:               true,
		TransientCooldown:     DefaultTransientCooldown,
		AuthCooldown:          DefaultAuthCooldown,
		VerificationCooldown:  DefaultVerificationCooldown,
		NotFoundCooldown:      DefaultNotFoundCooldown,
		MaxCooldown:           DefaultMaxCooldown,
		RetryAfterCap:         DefaultRetryAfterCap,
		RequestBudget:         DefaultRequestBudget,
		BestEffortWhenAllOpen: true,
	}
}

func (p Policy) normalized() Policy {
	def := DefaultPolicy()
	if p.TransientCooldown <= 0 {
		p.TransientCooldown = def.TransientCooldown
	}
	if p.AuthCooldown <= 0 {
		p.AuthCooldown = def.AuthCooldown
	}
	if p.VerificationCooldown <= 0 {
		p.VerificationCooldown = def.VerificationCooldown
	}
	if p.NotFoundCooldown <= 0 {
		p.NotFoundCooldown = def.NotFoundCooldown
	}
	if p.MaxCooldown <= 0 {
		p.MaxCooldown = def.MaxCooldown
	}
	if p.RetryAfterCap < 0 {
		p.RetryAfterCap = 0
	}
	if p.RequestBudget < 0 {
		p.RequestBudget = 0
	}
	return p
}

func (p Policy) allowsCooling(provider string) bool {
	if !p.Enabled || p.Disabled {
		return false
	}
	if p.DisabledProviders != nil {
		if _, ok := p.DisabledProviders[strings.ToLower(strings.TrimSpace(provider))]; ok {
			return false
		}
	}
	return true
}

// MemberHealth is the observable state of one combo member.
type MemberHealth struct {
	Provider       string      `json:"provider"`
	Model          string      `json:"model"`
	State          MemberState `json:"state"`
	Failures       int         `json:"failures"`
	LastStatusCode int         `json:"last_status_code,omitempty"`
	LastError      string      `json:"last_error,omitempty"`
	LastFailureAt  time.Time   `json:"last_failure_at,omitempty"`
	CooldownUntil  time.Time   `json:"cooldown_until,omitempty"`
	RetryAfter     string      `json:"retry_after,omitempty"`
	Probing        bool        `json:"probing,omitempty"`
}

type memberRecord struct {
	state         MemberState
	failures      int
	until         time.Time
	lastStatus    int
	lastErr       string
	lastFailureAt time.Time
	probing       bool
	probeAt       time.Time
}

type healthStore struct {
	mu          sync.Mutex
	policy      Policy
	records     map[string]*memberRecord
	fingerprint string
}

var health = healthStore{records: map[string]*memberRecord{}}

// MemberKey normalizes a combo member reference into its health key.
func MemberKey(provider, model string) string {
	return strings.ToLower(strings.TrimSpace(provider) + "/" + strings.TrimSpace(model))
}

// ConfigureHealth installs the policy and resets member state. Used by tests and
// by explicit operator resets.
func ConfigureHealth(policy Policy) {
	health.mu.Lock()
	health.policy = policy.normalized()
	health.fingerprint = ""
	health.records = map[string]*memberRecord{}
	health.mu.Unlock()
}

// SyncHealthPolicy installs the policy from config. State is only reset when the
// member universe (or the cooling opt-outs) actually changed, so an unrelated
// config reload does not throw away live cooldowns.
func SyncHealthPolicy(policy Policy, fingerprint string) {
	health.mu.Lock()
	defer health.mu.Unlock()
	changed := fingerprint != health.fingerprint
	health.policy = policy.normalized()
	health.fingerprint = fingerprint
	if changed {
		health.records = map[string]*memberRecord{}
	}
}

// AllowAttempt reports whether a member may be attempted now.
//
// A closed member is always allowed. An open member whose cooldown expired
// transitions to half-open and admits exactly one probe; every other request
// keeps skipping it until that probe resolves, which prevents a probe stampede.
func AllowAttempt(provider, model string, now time.Time) (bool, time.Duration) {
	health.mu.Lock()
	defer health.mu.Unlock()
	if !health.policy.allowsCooling(provider) {
		return true, 0
	}
	rec := health.records[MemberKey(provider, model)]
	if rec == nil {
		return true, 0
	}
	switch rec.state {
	case MemberOpen:
		if now.Before(rec.until) {
			return false, rec.until.Sub(now)
		}
		if rec.probing {
			// A probe that never reported back (handler panic, process fault) must
			// not block the member forever.
			if now.Sub(rec.probeAt) < probeLease {
				return false, 0
			}
			rec.probing = false
		}
		rec.probing = true
		rec.probeAt = now
		rec.state = MemberHalfOpen
		return true, 0
	case MemberHalfOpen:
		if rec.probing && now.Sub(rec.probeAt) < probeLease {
			return false, 0
		}
		rec.probing = true
		rec.probeAt = now
		return true, 0
	default:
		return true, 0
	}
}

// ForceAttempt marks a member as probed regardless of its cooldown. Used only
// for the best-effort rescue when every member of a chain is open.
func ForceAttempt(provider, model string) {
	health.mu.Lock()
	defer health.mu.Unlock()
	key := MemberKey(provider, model)
	rec := health.records[key]
	if rec == nil {
		rec = &memberRecord{}
		health.records[key] = rec
	}
	rec.state = MemberHalfOpen
	rec.probing = true
	rec.probeAt = time.Now()
}

// RecordSuccess closes a member and clears its failure history, so the primary
// member returns to the front of the chain on the next request.
func RecordSuccess(provider, model string) {
	health.mu.Lock()
	defer health.mu.Unlock()
	delete(health.records, MemberKey(provider, model))
}

// RecordFailure opens a member for a cooldown derived from the failure class and
// from how many consecutive failures it already has. It returns the applied
// cooldown (zero when the provider opts out of cooling).
func RecordFailure(provider, model string, status int, body []byte, now time.Time, retryHint ...time.Duration) time.Duration {
	health.mu.Lock()
	defer health.mu.Unlock()
	if !health.policy.allowsCooling(provider) {
		return 0
	}

	key := MemberKey(provider, model)
	rec := health.records[key]
	if rec == nil {
		rec = &memberRecord{state: MemberClosed}
		health.records[key] = rec
	}
	rec.probing = false
	rec.lastStatus = status
	rec.lastErr = truncateError(snippetBody(body))
	rec.lastFailureAt = now

	p := health.policy
	base := p.TransientCooldown
	switch {
	case IsVerificationRequired(status, body):
		base = p.VerificationCooldown
	case status == 404 || IsModelMissing(status, body):
		base = p.NotFoundCooldown
	case status == 401 || status == 403:
		base = p.AuthCooldown
	}

	// Exponential backoff across consecutive failures, always capped.
	cooldown := base
	for i := 0; i < rec.failures; i++ {
		cooldown *= 2
		if cooldown >= p.MaxCooldown {
			cooldown = p.MaxCooldown
			break
		}
	}
	// An upstream retry hint (a rate-limit window, a quota reset) is more accurate
	// than our backoff; honour it up to the retry cap so the member is not probed
	// every few minutes while it is legitimately unavailable.
	if len(retryHint) > 0 && retryHint[0] > cooldown {
		hint := retryHint[0]
		if p.RetryAfterCap > 0 && hint > p.RetryAfterCap {
			hint = p.RetryAfterCap
		}
		if hint > cooldown {
			cooldown = hint
		}
	}
	rec.failures++
	rec.state = MemberOpen
	rec.until = now.Add(cooldown)
	return cooldown
}

// HealthPolicy returns the active policy snapshot.
func HealthPolicy() Policy {
	health.mu.Lock()
	defer health.mu.Unlock()
	return health.policy
}

// SoonestCooling returns the chain member that recovers first, considering only
// members that are currently open or half-open.
func SoonestCooling(members []config.ComboModelRef, now time.Time) (config.ComboModelRef, time.Time, bool) {
	health.mu.Lock()
	defer health.mu.Unlock()
	var best config.ComboModelRef
	var bestUntil time.Time
	found := false
	for _, m := range members {
		rec := health.records[MemberKey(m.Provider, m.Model)]
		if rec == nil || rec.state == MemberClosed {
			continue
		}
		until := rec.until
		if until.Before(now) || rec.state == MemberHalfOpen {
			until = now
		}
		if !found || until.Before(bestUntil) {
			best, bestUntil, found = m, until, true
		}
	}
	return best, bestUntil, found
}

// SnapshotHealth returns the current member states for the management API.
func SnapshotHealth(now time.Time) []MemberHealth {
	health.mu.Lock()
	defer health.mu.Unlock()
	out := make([]MemberHealth, 0, len(health.records))
	for key, rec := range health.records {
		provider, model, _ := strings.Cut(key, "/")
		state := rec.state
		if state == MemberOpen && !now.Before(rec.until) {
			state = MemberHalfOpen
			rec.state = MemberHalfOpen
		}
		item := MemberHealth{
			Provider:       provider,
			Model:          model,
			State:          state,
			Failures:       rec.failures,
			LastStatusCode: rec.lastStatus,
			LastError:      rec.lastErr,
			LastFailureAt:  rec.lastFailureAt,
			Probing:        rec.probing,
		}
		if state != MemberClosed && rec.until.After(now) {
			item.CooldownUntil = rec.until
			item.RetryAfter = rec.until.Sub(now).Round(time.Second).String()
		}
		out = append(out, item)
	}
	return out
}

// ResetHealth clears every member state, or one member when provider/model are
// both provided.
func ResetHealth(provider, model string) {
	health.mu.Lock()
	defer health.mu.Unlock()
	if provider == "" && model == "" {
		health.records = map[string]*memberRecord{}
		return
	}
	delete(health.records, MemberKey(provider, model))
}

// MemberSnapshot returns the state for a single member (used in log lines and
// tests).
func MemberSnapshot(provider, model string, now time.Time) (MemberState, time.Duration) {
	health.mu.Lock()
	defer health.mu.Unlock()
	rec := health.records[MemberKey(provider, model)]
	if rec == nil || rec.state == MemberClosed {
		return MemberClosed, 0
	}
	if rec.state == MemberOpen && !now.Before(rec.until) {
		return MemberHalfOpen, 0
	}
	return rec.state, rec.until.Sub(now)
}

func snippetBody(body []byte) string {
	const limit = 240
	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}

func truncateError(s string) string {
	// Collapse newlines so management output stays one line per member.
	return strings.Join(strings.Fields(s), " ")
}
