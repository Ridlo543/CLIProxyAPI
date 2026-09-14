package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/antigravity"
	"github.com/tidwall/gjson"
)

// The panel's reasoning controls used to write a capability list that the
// request path never read, so "High" was shown and nothing was sent. These tests
// read the body that actually leaves for Antigravity.

const policyBudgetPath = "request.generationConfig.thinkingConfig.thinkingBudget"

func opusOnAntigravity() *registry.ModelInfo {
	// Mirrors the static catalog entry for claude-opus-4-6-thinking.
	return &registry.ModelInfo{
		ID:                  "claude-opus-4-6-thinking",
		Type:                "antigravity",
		MaxCompletionTokens: 64000,
		Thinking:            &registry.ThinkingSupport{Min: 1024, Max: 64000, ZeroAllowed: true, DynamicAllowed: true},
	}
}

func applyForAntigravity(t *testing.T, model string, body, source string) []byte {
	t.Helper()
	out, err := thinking.ApplyThinkingWithModelInfo([]byte(body), []byte(source), model, "openai", "antigravity", "antigravity", opusOnAntigravity())
	if err != nil {
		t.Fatalf("ApplyThinkingWithModelInfo() error = %v", err)
	}
	return out
}

func setPolicy(t *testing.T, policies map[string]thinking.ProviderPolicy) {
	t.Helper()
	thinking.SetReasoningPolicy(policies)
	t.Cleanup(func() { thinking.SetReasoningPolicy(nil) })
}

func TestReasoningPolicyUnsetLeavesTheRequestAlone(t *testing.T) {
	thinking.SetReasoningPolicy(nil)
	out := applyForAntigravity(t, "claude-opus-4-6-thinking", `{"request":{"generationConfig":{}}}`, `{}`)
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("no policy and no client config must send no thinkingConfig; body=%s", out)
	}
}

func TestReasoningPolicyProviderDefaultIsSent(t *testing.T) {
	setPolicy(t, map[string]thinking.ProviderPolicy{"antigravity": {Default: "12000"}})
	out := applyForAntigravity(t, "claude-opus-4-6-thinking", `{"request":{"generationConfig":{}}}`, `{}`)
	if got := gjson.GetBytes(out, policyBudgetPath).Int(); got != 12000 {
		t.Fatalf("thinkingBudget = %d, want the provider default 12000; body=%s", got, out)
	}
}

func TestReasoningPolicyModelEntryBeatsProviderDefault(t *testing.T) {
	setPolicy(t, map[string]thinking.ProviderPolicy{"antigravity": {
		Default: "12000",
		Models:  map[string]string{"claude-opus-4-6-thinking": "8000"},
	}})
	out := applyForAntigravity(t, "claude-opus-4-6-thinking", `{"request":{"generationConfig":{}}}`, `{}`)
	if got := gjson.GetBytes(out, policyBudgetPath).Int(); got != 8000 {
		t.Fatalf("thinkingBudget = %d, want the model entry 8000; body=%s", got, out)
	}
}

func TestReasoningPolicyModelWithoutEntryFallsBackToProvider(t *testing.T) {
	setPolicy(t, map[string]thinking.ProviderPolicy{"antigravity": {
		Default: "12000",
		Models:  map[string]string{"some-other-model": "2000"},
	}})
	out := applyForAntigravity(t, "claude-opus-4-6-thinking", `{"request":{"generationConfig":{}}}`, `{}`)
	if got := gjson.GetBytes(out, policyBudgetPath).Int(); got != 12000 {
		t.Fatalf("thinkingBudget = %d, want the provider default 12000; body=%s", got, out)
	}
}

func TestReasoningPolicyOverridesWhatTheClientSent(t *testing.T) {
	setPolicy(t, map[string]thinking.ProviderPolicy{"antigravity": {Models: map[string]string{"claude-opus-4-6-thinking": "8000"}}})
	out := applyForAntigravity(t, "claude-opus-4-6-thinking",
		`{"request":{"generationConfig":{"thinkingConfig":{"thinkingBudget":2048}}}}`,
		`{"reasoning_effort":"low"}`)
	if got := gjson.GetBytes(out, policyBudgetPath).Int(); got != 8000 {
		t.Fatalf("thinkingBudget = %d, want the forced 8000 over the client's low; body=%s", got, out)
	}
}

func TestReasoningPolicyOverridesTheModelSuffix(t *testing.T) {
	setPolicy(t, map[string]thinking.ProviderPolicy{"antigravity": {Default: "8000"}})
	out := applyForAntigravity(t, "claude-opus-4-6-thinking(1024)", `{"request":{"generationConfig":{}}}`, `{}`)
	if got := gjson.GetBytes(out, policyBudgetPath).Int(); got != 8000 {
		t.Fatalf("thinkingBudget = %d, want the forced 8000 over the (1024) suffix; body=%s", got, out)
	}
}

func TestReasoningPolicyNoneDisablesThinking(t *testing.T) {
	setPolicy(t, map[string]thinking.ProviderPolicy{"antigravity": {Default: "none"}})
	out := applyForAntigravity(t, "claude-opus-4-6-thinking",
		`{"request":{"generationConfig":{"thinkingConfig":{"thinkingBudget":20000}}}}`,
		`{"reasoning_effort":"high"}`)
	if got := gjson.GetBytes(out, policyBudgetPath); got.Exists() && got.Int() != 0 {
		t.Fatalf("thinkingBudget = %s, want thinking off; body=%s", got.Raw, out)
	}
}

func TestReasoningPolicyForAnotherProviderDoesNotLeak(t *testing.T) {
	setPolicy(t, map[string]thinking.ProviderPolicy{"codex": {Default: "high"}})
	out := applyForAntigravity(t, "claude-opus-4-6-thinking", `{"request":{"generationConfig":{}}}`, `{}`)
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("a codex policy changed an antigravity request; body=%s", out)
	}
}

func TestReasoningPolicyLevelIsClampedToTheModel(t *testing.T) {
	// Opus on Antigravity takes a budget, not a level; the policy still has to
	// go through validation instead of being written raw.
	setPolicy(t, map[string]thinking.ProviderPolicy{"antigravity": {Default: "high"}})
	out := applyForAntigravity(t, "claude-opus-4-6-thinking", `{"request":{"generationConfig":{}}}`, `{}`)
	budget := gjson.GetBytes(out, policyBudgetPath).Int()
	if budget < 1024 || budget >= 64000 {
		t.Fatalf("thinkingBudget = %d, want a valid budget for high inside [1024, 64000); body=%s", budget, out)
	}
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").Exists() {
		t.Fatalf("a level was written raw for a budget-only model; body=%s", out)
	}
}
