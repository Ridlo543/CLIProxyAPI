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
