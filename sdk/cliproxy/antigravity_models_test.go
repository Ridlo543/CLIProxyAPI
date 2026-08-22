package cliproxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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
	calls := 0
	rt := antigravityModelRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":{"gemini-pool":{"displayName":"Pool"}}}`))}, nil
	})
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRoundTripperProvider(antigravityModelTransportProvider{rt: rt})
	service := &Service{coreManager: manager}
	hints := service.fetchAntigravityModelCapabilityHintsForAuth(t.Context(), &coreauth.Auth{ProxyPool: "office", Metadata: map[string]any{"access_token": "token"}})
	if calls != 1 || len(hints.Models) != 1 || hints.Models[0].ID != "gemini-pool" {
		t.Fatalf("calls=%d hints=%#v", calls, hints)
	}
}

func TestParseAntigravityModelCapabilityHintsIncludesAvailableModels(t *testing.T) {
	hints := parseAntigravityModelCapabilityHints([]byte(`{
		"models": {
			"gemini-new": {"displayName":"Gemini New","maxTokens":123,"maxOutputTokens":45},
			"chat_20706": {"displayName":"Internal"}
		},
		"webSearchModelIds": ["gemini-new"]
	}`))

	if len(hints.Models) != 1 {
		t.Fatalf("models count = %d, want 1", len(hints.Models))
	}
	model := hints.Models[0]
	if model.ID != "gemini-new" || model.ContextLength != 123 || model.MaxCompletionTokens != 45 {
		t.Fatalf("unexpected model: %#v", model)
	}
}

func TestApplyAntigravityFetchedModelCapabilitiesMergesModels(t *testing.T) {
	existing := &ModelInfo{ID: "gemini-known", DisplayName: "Old", ContextLength: 1}
	hints := antigravityModelCapabilityHints{
		Models: []*ModelInfo{
			{ID: "gemini-known", DisplayName: "Current", ContextLength: 100},
			{ID: "gemini-new", DisplayName: "New"},
		},
		WebSearchModelIDs: map[string]struct{}{"gemini-new": {}},
	}

	models := applyAntigravityFetchedModelCapabilities([]*ModelInfo{existing}, hints)
	if len(models) != 2 {
		t.Fatalf("models count = %d, want 2", len(models))
	}
	if existing.DisplayName != "Old" || existing.ContextLength != 1 {
		t.Fatalf("static model mutated: %#v", existing)
	}
	if models[0] == existing || models[0].DisplayName != "Current" || models[0].ContextLength != 1 {
		t.Fatalf("merged model should update display metadata but preserve static limits: %#v", models[0])
	}
	if !models[1].SupportsWebSearch {
		t.Fatal("new fetched model should support web search")
	}
}
