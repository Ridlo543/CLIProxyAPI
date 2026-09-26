package apikeypolicy

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func newTestEnforcer(policies []config.APIKeyPolicy, now func() time.Time) *Enforcer {
	e := NewEnforcer()
	e.SetClock(now)
	e.Replace(policies)
	return e
}

// NewEnforcer builds an isolated enforcer (tests); production uses Default().
func NewEnforcer() *Enforcer { return &Enforcer{nowClock: time.Now} }

func TestCheckModelAndProviders(t *testing.T) {
	e := newTestEnforcer([]config.APIKeyPolicy{{
		Key:       "k",
		Models:    []string{"gpt-a"},
		Providers: []string{"Codex"},
	}}, nil)

	if !e.CheckModel("k", "gpt-a") || e.CheckModel("k", "other") {
		t.Fatal("model allowlist failed")
	}
	if e.CheckModel("unknown-key", "anything") != true {
		t.Fatal("no policy must allow all models")
	}
	if !e.CheckProviders("k", []string{"codex", "xai"}, true) {
		t.Fatal("provider intersection should pass")
	}
	if e.CheckProviders("k", []string{"gemini"}, true) {
		t.Fatal("non-intersecting providers must fail")
	}
	if e.CheckProviders("k", nil, false) {
		t.Fatal("unknown model must be denied when provider restriction set")
	}
	if !e.CheckProviders("k", nil, false) && len(e.mustPolicy(t, "k").Providers) == 0 {
		t.Fatal("unreachable")
	}
}

func TestCheckModel_NamespacedAndCrossProvider(t *testing.T) {
	e := newTestEnforcer([]config.APIKeyPolicy{
		{
			Key:    "key-namespaced",
			Models: []string{"openagentic/gemini-2.5-flash", "gpt-5.6-sol"},
		},
	}, nil)

	// 1. Exact match on qualified ID
	if !e.CheckModel("key-namespaced", "openagentic/gemini-2.5-flash") {
		t.Fatal("expected openagentic/gemini-2.5-flash to pass")
	}
	// 2. Reject different provider with same model name
	if e.CheckModel("key-namespaced", "antigravity/gemini-2.5-flash") {
		t.Fatal("expected antigravity/gemini-2.5-flash to be rejected")
	}
	// 3. Bare model matches if allowed has provider qualification
	if !e.CheckModel("key-namespaced", "gemini-2.5-flash") {
		t.Fatal("expected bare gemini-2.5-flash to pass")
	}
	// 4. Pinned provider helper
	if prov := e.PinnedProviderForModel("key-namespaced", "gemini-2.5-flash"); prov != "openagentic" {
		t.Fatalf("expected pinned provider openagentic, got %q", prov)
	}
	// 5. Bare allowed model matches both bare and qualified request
	if !e.CheckModel("key-namespaced", "gpt-5.6-sol") {
		t.Fatal("expected gpt-5.6-sol to pass")
	}
	if !e.CheckModel("key-namespaced", "codex/gpt-5.6-sol") {
		t.Fatal("expected codex/gpt-5.6-sol to pass")
	}
}

func TestIsModelAllowed(t *testing.T) {
	e := newTestEnforcer([]config.APIKeyPolicy{
		{
			Key:       "key-filter",
			Models:    []string{"openagentic/gemini-2.5-flash", "gpt-5.6-sol"},
			Providers: []string{"openagentic", "codex"},
		},
	}, nil)

	// Allowed: openagentic/gemini-2.5-flash
	if !e.IsModelAllowed("key-filter", "openagentic/gemini-2.5-flash", "openagentic") {
		t.Fatal("expected openagentic/gemini-2.5-flash to be allowed")
	}
	// Allowed: bare gemini-2.5-flash if owned by openagentic
	if !e.IsModelAllowed("key-filter", "gemini-2.5-flash", "openagentic") {
		t.Fatal("expected gemini-2.5-flash (owned by openagentic) to be allowed")
	}
	// Disallowed: antigravity/gemini-2.5-flash
	if e.IsModelAllowed("key-filter", "antigravity/gemini-2.5-flash", "antigravity") {
		t.Fatal("expected antigravity/gemini-2.5-flash to be disallowed")
	}
	// Disallowed: gemini-2.5-flash if owned by antigravity
	if e.IsModelAllowed("key-filter", "gemini-2.5-flash", "antigravity") {
		t.Fatal("expected gemini-2.5-flash (owned by antigravity) to be disallowed")
	}
	// Allowed: gpt-5.6-sol owned by codex
	if !e.IsModelAllowed("key-filter", "gpt-5.6-sol", "codex") {
		t.Fatal("expected gpt-5.6-sol to be allowed")
	}
	// Disallowed: claude-sonnet-4-6 owned by anthropic/antigravity
	if e.IsModelAllowed("key-filter", "claude-sonnet-4-6", "antigravity") {
		t.Fatal("expected claude-sonnet-4-6 to be disallowed")
	}
}

func TestBudgetAndWindowResetWithInjectedClock(t *testing.T) {
	current := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return current }
	e := newTestEnforcer([]config.APIKeyPolicy{{Key: "k", Limit: &config.APIKeyTokenLimit{Window: "daily", Limit: 100}}}, now)

	for i := 0; i < 99; i++ {
		e.Record("k", 1)
	}
	if !e.CheckBudget("k") {
		t.Fatal("budget should remain under limit")
	}
	e.Record("k", 5)
	if e.CheckBudget("k") {
		t.Fatal("budget exceeded must be denied")
	}
	usage := e.UsageSnapshot()
	if len(usage) != 1 || usage[0].TokensUsed != 104 {
		t.Fatalf("usage=%+v", usage)
	}
	current = current.Add(14 * time.Hour)
	if !e.CheckBudget("k") {
		t.Fatal("UTC midnight rollover should reset budget")
	}
	usage = e.UsageSnapshot()
	if usage[0].TokensUsed != 0 || usage[0].WindowStart != current.UTC() {
		t.Fatalf("post-rollover usage=%+v", usage)
	}
}

func TestMonthlyAndTotalWindows(t *testing.T) {
	current := time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC)
	now := func() time.Time { return current }
	e := newTestEnforcer([]config.APIKeyPolicy{
		{Key: "m", Limit: &config.APIKeyTokenLimit{Window: "monthly", Limit: 10}},
		{Key: "t", Limit: &config.APIKeyTokenLimit{Window: "total", Limit: 10}},
	}, now)
	e.Record("m", 10)
	e.Record("t", 10)
	if e.CheckBudget("m") || e.CheckBudget("t") {
		t.Fatal("both budgets exhausted")
	}
	current = current.Add(2 * time.Hour) // cross UTC month boundary
	if !e.CheckBudget("m") {
		t.Fatal("monthly reset expected")
	}
	if e.CheckBudget("t") {
		t.Fatal("total window must not reset")
	}
}

func (e *Enforcer) mustPolicy(t *testing.T, key string) config.APIKeyPolicy {
	t.Helper()
	p, _ := e.Policy(key)
	return p
}

func TestUsageSnapshotMasksKeys(t *testing.T) {
	e := newTestEnforcer(nil, nil)
	_ = e
	def := Default()
	def.Replace(nil)
	def.Record("sk-ainy-abcdef", 5)
	snap := def.UsageSnapshot()
	if len(snap) == 0 || !strings.HasSuffix(snap[0].KeyMasked, "cdef") {
		t.Fatalf("snapshot=%+v", snap)
	}
}
