package cliproxy

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type antigravityModelTransportProvider struct{ rt http.RoundTripper }

func (p antigravityModelTransportProvider) RoundTripperFor(*coreauth.Auth) http.RoundTripper {
	return p.rt
}

type antigravityModelRoundTripper func(*http.Request) (*http.Response, error)

func (f antigravityModelRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAntigravityModelsRequestPayload(t *testing.T) {
	auth := &coreauth.Auth{Metadata: map[string]any{"project_id": `project-"quoted"`}}
	got := antigravityModelsRequestPayload(auth)
	want := "{\"project\":\"project-\\\"quoted\\\"\"}"
	if got != want {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}

func TestAntigravityModelDiscoveryUsesAuthResolvedTransport(t *testing.T) {
	// The probe fans out across the candidate base URLs concurrently, so the counter
	// is atomic and the assertion only requires that the auth-resolved transport was
	// the one used, not how many endpoints were raced.
	var calls atomic.Int64
	rt := antigravityModelRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":{"gemini-pool":{"displayName":"Pool"}}}`))}, nil
	})
	auth := &coreauth.Auth{ID: "auth-1", ProxyPool: "office", Metadata: map[string]any{"access_token": "token"}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRoundTripperProvider(antigravityModelTransportProvider{rt: rt})
	_, _ = manager.Register(t.Context(), auth)
	service := &Service{coreManager: manager}
	hints := service.fetchAntigravityModelCapabilityHintsForAuth(t.Context(), auth)
	if calls.Load() == 0 || hints.ModelIDs == nil {
		t.Fatalf("calls=%d hints=%#v", calls.Load(), hints)
	}
	if _, ok := hints.ModelIDs["gemini-pool"]; !ok {
		t.Fatalf("expected gemini-pool in hints.ModelIDs: %#v", hints.ModelIDs)
	}
}

func TestParseAntigravityModelCapabilityHintsIncludesAvailableModels(t *testing.T) {
	hints, ok := parseAntigravityModelCapabilityHints([]byte(`{
		"models": {
			"gemini-new": {"displayName":"Gemini New","maxTokens":123,"maxOutputTokens":45},
			"chat_20706": {"displayName":"Internal"}
		},
		"webSearchModelIds": ["gemini-new"]
	}`))

	if !ok {
		t.Fatal("parse failed")
	}
	if len(hints.ModelIDs) != 2 {
		t.Fatalf("models count = %d, want 2", len(hints.ModelIDs))
	}
	if _, ok := hints.ModelIDs["gemini-new"]; !ok {
		t.Fatalf("expected gemini-new in hints.ModelIDs: %#v", hints.ModelIDs)
	}
}

func TestApplyAntigravityFetchedModelCapabilitiesMergesModels(t *testing.T) {
	existing := &ModelInfo{ID: "gemini-known", DisplayName: "Old", ContextLength: 1}
	hints := antigravityModelCapabilityHints{
		ModelIDs:          map[string]struct{}{"gemini-known": {}},
		WebSearchModelIDs: map[string]struct{}{"gemini-known": {}},
	}

	models := applyAntigravityFetchedModelCapabilities([]*ModelInfo{existing}, hints)
	if len(models) != 1 {
		t.Fatalf("models count = %d, want 1", len(models))
	}
	if !models[0].SupportsWebSearch {
		t.Fatal("expected model to support web search")
	}
}

// Upstream added a capability cache whose clone kept only WebSearchModelIDs, so
// every probe after the first returned no live catalog for the account.
func TestAntigravityCachedProbeKeepsAvailableModels(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)
	var calls atomic.Int32
	rt := antigravityModelRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":{"gemini-cached":{"displayName":"Cached"}}}`))}, nil
	})
	auth := &coreauth.Auth{ID: "cached-probe", Metadata: map[string]any{"access_token": "token", "project_id": "proj-cached"}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRoundTripperProvider(antigravityModelTransportProvider{rt: rt})
	_, _ = manager.Register(t.Context(), auth)
	service := &Service{coreManager: manager}

	for i := 0; i < 2; i++ {
		hints := service.fetchAntigravityModelCapabilityHintsForAuth(t.Context(), auth)
		if len(hints.ModelIDs) != 1 {
			t.Fatalf("probe %d: modelIDs = %#v, want the live catalog", i+1, hints.ModelIDs)
		}
		if _, ok := hints.ModelIDs["gemini-cached"]; !ok {
			t.Fatalf("probe %d: expected gemini-cached in modelIDs", i+1)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (second probe served from cache)", calls.Load())
	}
}

func TestAntigravityProbeCacheIsScopedToTheProject(t *testing.T) {
	a := &coreauth.Auth{Metadata: map[string]any{"project_id": "proj-a"}}
	b := &coreauth.Auth{Metadata: map[string]any{"project_id": "proj-b"}}
	if antigravityModelsRequestPayload(a) == antigravityModelsRequestPayload(b) {
		t.Fatal("different projects produced the same probe payload, so they would share a cache entry")
	}
}
func TestFilterAntigravityModels_RetainsClaude55ForClaudeEntitledAccount(t *testing.T) {
	catalog := []*ModelInfo{
		{ID: "claude-opus-4-6-thinking"},
		{ID: "claude-sonnet-4-6"},
		{ID: "claude-opus-5-5-high"},
		{ID: "claude-opus-5-5-medium"},
		{ID: "claude-opus-5-5-low"},
		{ID: "claude-sonnet-5-5-high"},
		{ID: "gemini-3.8-flash-high"},
		{ID: "gemini-2.5-flash"},
	}

	// 1. Account with Claude Opus entitlement: must retain Opus 5.5 variants even if probe only returned 4.6
	hintsOpus := antigravityModelCapabilityHints{
		ModelIDs: map[string]struct{}{
			"claude-opus-4-6-thinking": {},
			"gemini-3.8-flash-high":    {},
		},
	}
	filtered := filterAntigravityModels(catalog, hintsOpus)
	ids := make(map[string]bool)
	for _, m := range filtered {
		ids[m.ID] = true
	}

	if !ids["claude-opus-4-6-thinking"] {
		t.Error("expected claude-opus-4-6-thinking to be kept")
	}
	if !ids["claude-opus-5-5-high"] {
		t.Error("expected claude-opus-5-5-high to be kept for Claude Opus entitled account")
	}
	if !ids["claude-opus-5-5-medium"] {
		t.Error("expected claude-opus-5-5-medium to be kept for Claude Opus entitled account")
	}
	if ids["claude-sonnet-5-5-high"] {
		t.Error("did not expect claude-sonnet-5-5-high when account has no Sonnet entitlement")
	}
	if !ids["gemini-3.8-flash-high"] {
		t.Error("expected gemini-3.8-flash-high to be kept")
	}
	if ids["gemini-2.5-flash"] {
		t.Error("did not expect gemini-2.5-flash when not in hints")
	}

	// 2. Account with Claude Sonnet entitlement: must retain Sonnet 5.5 variants
	hintsSonnet := antigravityModelCapabilityHints{
		ModelIDs: map[string]struct{}{
			"claude-sonnet-4-6": {},
		},
	}
	filteredSonnet := filterAntigravityModels(catalog, hintsSonnet)
	idsSonnet := make(map[string]bool)
	for _, m := range filteredSonnet {
		idsSonnet[m.ID] = true
	}
	if !idsSonnet["claude-sonnet-4-6"] || !idsSonnet["claude-sonnet-5-5-high"] {
		t.Error("expected claude-sonnet-4-6 and claude-sonnet-5-5-high to be kept")
	}
	if idsSonnet["claude-opus-5-5-high"] {
		t.Error("did not expect claude-opus-5-5-high when account only has Sonnet")
	}
}
