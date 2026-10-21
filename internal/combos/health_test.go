package combos

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func resetHealth(t *testing.T) {
	t.Helper()
	ConfigureHealth(DefaultPolicy())
	t.Cleanup(func() { ConfigureHealth(DefaultPolicy()) })
}

func TestMemberBreaker_SkipsWhileOpenAndProbesOnceAfterCooldown(t *testing.T) {
	resetHealth(t)
	now := time.Now()

	cooldown := RecordFailure("antigravity", "gemini-3.8-flash-high", http.StatusServiceUnavailable, []byte(`{"error":{"message":"overloaded"}}`), now)
	if cooldown != DefaultTransientCooldown {
		t.Fatalf("transient cooldown = %s, want %s", cooldown, DefaultTransientCooldown)
	}

	allowed, remaining := AllowAttempt("antigravity", "gemini-3.8-flash-high", now.Add(time.Second))
	if allowed {
		t.Fatal("open member must be skipped")
	}
	if remaining <= 0 || remaining > cooldown {
		t.Fatalf("remaining = %s, want (0, %s]", remaining, cooldown)
	}

	after := now.Add(cooldown + time.Millisecond)
	allowed, _ = AllowAttempt("antigravity", "gemini-3.8-flash-high", after)
	if !allowed {
		t.Fatal("expired cooldown must admit one probe")
	}
	allowed, _ = AllowAttempt("antigravity", "gemini-3.8-flash-high", after)
	if allowed {
		t.Fatal("second concurrent request must not probe the same member")
	}

	// A successful probe closes the member immediately: the primary member is
	// back in rotation on the very next request.
	RecordSuccess("antigravity", "gemini-3.8-flash-high")
	allowed, _ = AllowAttempt("antigravity", "gemini-3.8-flash-high", after)
	if !allowed {
		t.Fatal("recorded success must close the breaker")
	}
	if state, _ := MemberSnapshot("antigravity", "gemini-3.8-flash-high", after); state != MemberClosed {
		t.Fatalf("state = %s, want closed", state)
	}
}

func TestMemberBreaker_FailedProbeDoublesCooldownUpToCap(t *testing.T) {
	resetHealth(t)
	now := time.Now()

	first := RecordFailure("codex", "gpt-6-luna", http.StatusInternalServerError, []byte("boom"), now)
	second := RecordFailure("codex", "gpt-6-luna", http.StatusInternalServerError, []byte("boom"), now.Add(time.Second))
	if second != first*2 {
		t.Fatalf("second cooldown = %s, want %s", second, first*2)
	}

	var last time.Duration
	for i := 0; i < 10; i++ {
		last = RecordFailure("codex", "gpt-6-luna", http.StatusInternalServerError, []byte("boom"), now)
	}
	if last > DefaultMaxCooldown {
		t.Fatalf("cooldown = %s, want <= %s", last, DefaultMaxCooldown)
	}
}

func TestMemberBreaker_ClassifiesVerificationAndNotFound(t *testing.T) {
	resetHealth(t)
	now := time.Now()

	verify := RecordFailure("antigravity", "claude-opus-4-6-thinking", http.StatusForbidden,
		[]byte(`{"error":{"message":"Verify your account to continue: https://accounts.google.com/..."}}`), now)
	if verify != DefaultVerificationCooldown {
		t.Fatalf("verification cooldown = %s, want %s", verify, DefaultVerificationCooldown)
	}

	missing := RecordFailure("jerouter", "ghost-model", http.StatusNotFound, []byte(`{"error":"model_not_found"}`), now)
	if missing != DefaultNotFoundCooldown {
		t.Fatalf("not-found cooldown = %s, want %s", missing, DefaultNotFoundCooldown)
	}
}

func TestMemberBreaker_RespectsProviderDisableCooling(t *testing.T) {
	p := DefaultPolicy()
	p.DisabledProviders = map[string]struct{}{"jerouter": {}}
	ConfigureHealth(p)
	t.Cleanup(func() { ConfigureHealth(DefaultPolicy()) })
	now := time.Now()

	cooldown := RecordFailure("jerouter", "step-5-unrestricted", http.StatusBadGateway, []byte("bad gateway"), now)
	if cooldown != 0 {
		t.Fatalf("cooldown = %s, want 0 for provider that opts out", cooldown)
	}
	if allowed, _ := AllowAttempt("jerouter", "step-5-unrestricted", now); !allowed {
		t.Fatal("provider that opts out must never be skipped")
	}
	if state, _ := MemberSnapshot("jerouter", "step-5-unrestricted", now); state != MemberClosed {
		t.Fatalf("state = %s, want closed", state)
	}
}

func TestMemberBreaker_SoonerRecoveryWinsBestEffortProbe(t *testing.T) {
	resetHealth(t)
	now := time.Now()
	RecordFailure("a", "slow", http.StatusServiceUnavailable, []byte("x"), now)
	RecordFailure("b", "fast", http.StatusServiceUnavailable, []byte("x"), now.Add(-25*time.Second))

	member, until, ok := SoonestCooling([]config.ComboModelRef{
		{Provider: "a", Model: "slow"},
		{Provider: "b", Model: "fast"},
	}, now)
	if !ok || member.Model != "fast" {
		t.Fatalf("rescue member = %+v, want b/fast", member)
	}
	if until.After(now.Add(6 * time.Second)) {
		t.Fatalf("until = %s, want ~5s", until.Sub(now))
	}
}

func TestShouldFallback_ContextAndPoisonClassification(t *testing.T) {
	contextBody := []byte(`{"error":{"message":"This model's maximum context length is 128000 tokens. context_length_exceeded"}}`)
	if !ShouldFallback(http.StatusBadRequest, contextBody) {
		t.Fatal("context overflow must fall through to a larger member")
	}
	if IsPoisonRequest(http.StatusBadRequest, contextBody) {
		t.Fatal("context overflow is not a poison request")
	}

	poison := []byte(`{"error":{"message":"tools.7.custom.input_schema: JSON schema is invalid"}}`)
	if ShouldFallback(http.StatusBadRequest, poison) {
		t.Fatal("invalid tool schema must not fall through")
	}
	if !IsPoisonRequest(http.StatusBadRequest, poison) {
		t.Fatal("invalid tool schema must be classified as poison")
	}
}

func TestExtractRetryAfter_HeaderAndBody(t *testing.T) {
	if d := ExtractRetryAfter(http.StatusTooManyRequests, "120", nil); d != 120*time.Second {
		t.Fatalf("header seconds = %s, want 2m", d)
	}
	if d := ExtractRetryAfter(http.StatusForbidden, "", []byte(`{"error":{"retry-after-ms":1799000}}`)); d != 1799*time.Second {
		t.Fatalf("body ms = %s, want 1799s", d)
	}
	if d := ExtractRetryAfter(http.StatusBadGateway, "", []byte(`{"error":"plain"}`)); d != 0 {
		t.Fatalf("absent hint = %s, want 0", d)
	}
	if d := ExtractRetryAfter(http.StatusOK, "120", nil); d != 0 {
		t.Fatalf("success must not carry a retry hint, got %s", d)
	}
}

func TestRecordFailure_HonorsUpstreamRetryHint(t *testing.T) {
	ConfigureHealth(DefaultPolicy())
	now := time.Now()
	got := RecordFailure("p", "m", http.StatusTooManyRequests, []byte(`{"error":"rate limited"}`), now, 10*time.Minute)
	if got != 10*time.Minute {
		t.Fatalf("cooldown = %s, want the 10m upstream hint", got)
	}
	ResetHealth("p", "m")

	ConfigureHealth(DefaultPolicy())
	got = RecordFailure("p", "m", http.StatusTooManyRequests, []byte(`{"error":"rate limited"}`), now, 2*time.Hour)
	if got != DefaultRetryAfterCap {
		t.Fatalf("cooldown = %s, want the %s cap", got, DefaultRetryAfterCap)
	}
}

func TestAllowAttempt_StaleProbeLeaseReopensTheGate(t *testing.T) {
	ConfigureHealth(DefaultPolicy())
	now := time.Now()
	RecordFailure("p", "m", http.StatusBadGateway, []byte("boom"), now)
	first := now.Add(DefaultTransientCooldown + time.Second)

	if ok, _ := AllowAttempt("p", "m", first); !ok {
		t.Fatal("expired cooldown must admit a probe")
	}
	if ok, _ := AllowAttempt("p", "m", first); ok {
		t.Fatal("a member with an in-flight probe must not be attempted twice")
	}
	if ok, _ := AllowAttempt("p", "m", first.Add(probeLease+time.Second)); !ok {
		t.Fatal("an abandoned probe must not block the member forever")
	}
}
