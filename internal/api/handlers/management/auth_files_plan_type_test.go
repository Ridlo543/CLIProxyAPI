package management

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Antigravity states its tier only through loadCodeAssist, so nothing is stored
// on disk for most credentials and every panel surface rendered no plan at all.
// The executor already caches the tier while refreshing AI credits.

func TestAuthPlanTypeUsesLiveAntigravityTier(t *testing.T) {
	const authID = "auth-antigravity-plan-live"
	coreauth.SetAntigravityCreditsHint(authID, coreauth.AntigravityCreditsHint{
		Known:      true,
		PaidTierID: "g1-pro-tier",
	})
	t.Cleanup(func() { coreauth.SetAntigravityCreditsHint(authID, coreauth.AntigravityCreditsHint{}) })

	auth := &coreauth.Auth{ID: authID, Provider: "antigravity"}
	if got := authPlanType(auth); got != "g1-pro-tier" {
		t.Fatalf("authPlanType = %q, want the live tier %q", got, "g1-pro-tier")
	}
}

func TestAuthPlanTypePrefersTheStoredValue(t *testing.T) {
	const authID = "auth-antigravity-plan-stored"
	coreauth.SetAntigravityCreditsHint(authID, coreauth.AntigravityCreditsHint{
		Known:      true,
		PaidTierID: "g1-pro-tier",
	})
	t.Cleanup(func() { coreauth.SetAntigravityCreditsHint(authID, coreauth.AntigravityCreditsHint{}) })

	auth := &coreauth.Auth{
		ID:         authID,
		Provider:   "antigravity",
		Attributes: map[string]string{"plan_type": "Google AI Ultra"},
	}
	if got := authPlanType(auth); got != "Google AI Ultra" {
		t.Fatalf("authPlanType = %q, want the operator's stored value", got)
	}
}

func TestAuthPlanTypeReportsNothingRatherThanGuessing(t *testing.T) {
	auth := &coreauth.Auth{ID: "auth-antigravity-plan-unknown", Provider: "antigravity"}
	if got := authPlanType(auth); got != "" {
		t.Fatalf("authPlanType = %q, want empty when no tier is known", got)
	}
}

func TestAuthPlanTypeLeavesOtherProvidersToTheirOwnMetadata(t *testing.T) {
	const authID = "auth-gemini-plan"
	// A stale antigravity hint under the same id must not leak into another
	// provider's row.
	coreauth.SetAntigravityCreditsHint(authID, coreauth.AntigravityCreditsHint{
		Known:      true,
		PaidTierID: "g1-pro-tier",
	})
	t.Cleanup(func() { coreauth.SetAntigravityCreditsHint(authID, coreauth.AntigravityCreditsHint{}) })

	auth := &coreauth.Auth{ID: authID, Provider: "gemini"}
	if got := authPlanType(auth); got != "" {
		t.Fatalf("authPlanType = %q, want empty for a non-antigravity provider", got)
	}
}
