package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// softRateLimitResult models a 429 that carries no quota watermark at all: the
// bare Google "RESOURCE_EXHAUSTED" body Antigravity returns for rejections that
// have nothing to do with the credential's remaining quota.
func softRateLimitResult(authID, model string) Result {
	return Result{
		AuthID:   authID,
		Provider: "antigravity",
		Model:    model,
		Success:  false,
		Error: &Error{
			Code:       ErrorCodeUpstreamRateLimit,
			Message:    `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`,
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
		},
		Options: cliproxyexecutor.Options{},
	}
}

func registerSoftRateLimitAuth(t *testing.T, manager *Manager, id string) {
	t.Helper()
	auth := &Auth{
		ID:       id,
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
}

// A 429 without any quota signal must cool the model briefly so the router stops
// hammering upstream, but it must not be recorded as exhausted quota: doing so
// mislabels a healthy credential in the management panel and puts it on the
// escalating quota ladder that can reach 30 minutes.
func TestMarkResultSoftRateLimitDoesNotRecordQuotaExhaustion(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	registerSoftRateLimitAuth(t, manager, "auth-soft-429")

	manager.MarkResult(context.Background(), softRateLimitResult("auth-soft-429", "gemini-3.8-flash-high"))

	got, ok := manager.GetByID("auth-soft-429")
	if !ok || got == nil {
		t.Fatalf("expected auth after MarkResult")
	}
	state := got.ModelStates["gemini-3.8-flash-high"]
	if state == nil {
		t.Fatalf("expected model state after MarkResult")
	}

	if state.Quota.Exceeded {
		t.Errorf("Quota.Exceeded = true, want false for a 429 carrying no quota signal")
	}
	if state.Quota.Reason == "quota" {
		t.Errorf("Quota.Reason = %q, want a non-quota reason", state.Quota.Reason)
	}
	if state.Quota.BackoffLevel != 0 {
		t.Errorf("Quota.BackoffLevel = %d, want 0 (no quota ladder)", state.Quota.BackoffLevel)
	}
	if !state.Quota.NextRecoverAt.IsZero() {
		t.Errorf("Quota.NextRecoverAt = %v, want zero", state.Quota.NextRecoverAt)
	}

	// The credential must still back off briefly, and that pause must stay short.
	if !state.NextRetryAfter.After(time.Now()) {
		t.Errorf("NextRetryAfter = %v, want a short cooldown in the future", state.NextRetryAfter)
	}
	if wait := time.Until(state.NextRetryAfter); wait > transientErrorCooldown+time.Minute {
		t.Errorf("cooldown of %v is longer than the bounded soft rate-limit pause", wait)
	}

	// The whole credential must not be taken down for one model's rejection.
	if got.Quota.Exceeded {
		t.Errorf("credential Quota.Exceeded = true, want false")
	}
}

// Repeated soft rate limits must not climb the quota backoff ladder, otherwise a
// retry burst across the pool escalates every credential toward the 30 minute cap.
func TestMarkResultSoftRateLimitDoesNotEscalate(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	registerSoftRateLimitAuth(t, manager, "auth-soft-429-repeat")

	for i := 0; i < 5; i++ {
		manager.MarkResult(context.Background(), softRateLimitResult("auth-soft-429-repeat", "gemini-3.8-flash-high"))
	}

	got, ok := manager.GetByID("auth-soft-429-repeat")
	if !ok || got == nil || got.ModelStates["gemini-3.8-flash-high"] == nil {
		t.Fatalf("expected model state after repeated MarkResult")
	}
	state := got.ModelStates["gemini-3.8-flash-high"]

	if state.Quota.BackoffLevel != 0 {
		t.Errorf("Quota.BackoffLevel = %d after 5 soft rate limits, want 0", state.Quota.BackoffLevel)
	}
	if state.Quota.Exceeded {
		t.Errorf("Quota.Exceeded = true after repeated soft rate limits, want false")
	}
	if wait := time.Until(state.NextRetryAfter); wait > transientErrorCooldown+time.Minute {
		t.Errorf("cooldown grew to %v across repeats, want it to stay bounded", wait)
	}
}

// A 429 that does carry a real quota signal keeps the existing behaviour.
func TestMarkResultQuotaSignalStillRecordsExhaustion(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	registerSoftRateLimitAuth(t, manager, "auth-real-quota")

	manager.MarkResult(context.Background(), quotaResult("auth-real-quota", "gemini-3.8-flash-high"))

	got, ok := manager.GetByID("auth-real-quota")
	if !ok || got == nil || got.ModelStates["gemini-3.8-flash-high"] == nil {
		t.Fatalf("expected model state after MarkResult")
	}
	state := got.ModelStates["gemini-3.8-flash-high"]

	if !state.Quota.Exceeded {
		t.Errorf("Quota.Exceeded = false, want true for a genuine quota 429")
	}
	if state.Quota.Reason != "quota" {
		t.Errorf("Quota.Reason = %q, want \"quota\"", state.Quota.Reason)
	}
	if state.Quota.BackoffLevel != 1 {
		t.Errorf("Quota.BackoffLevel = %d, want 1", state.Quota.BackoffLevel)
	}
}
