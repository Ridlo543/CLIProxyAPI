package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/combos"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestRewriteModelFieldSetsMemberModel(t *testing.T) {
	out, err := rewriteModelField([]byte(`{"model":"leader","messages":[{"role":"user","content":"hi"}]}`), "m-shared")
	if err != nil {
		t.Fatalf("rewriteModelField: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(top["model"]) != `"m-shared"` {
		t.Fatalf("model = %s, want \"m-shared\"", top["model"])
	}
	var msgs []map[string]string
	if err := json.Unmarshal(top["messages"], &msgs); err != nil || len(msgs) != 1 {
		t.Fatalf("messages lost: %v %v", msgs, err)
	}
}

func TestCombosAugmentModelsWritesSingleJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	combos.SyncFromConfig(&config.Config{Combos: []config.ComboConfig{
		{Name: "c1", Strategy: config.ComboStrategyFallback, Models: []config.ComboModelRef{{Provider: "p", Model: "m"}}},
	}})
	defer combos.SyncFromConfig(&config.Config{})

	r := gin.New()
	s := &Server{}
	r.GET("/v1/models", s.combosAugmentModels(func(c *gin.Context) {
		c.JSON(200, gin.H{"object": "list", "data": []gin.H{{"id": "real-1"}}})
	}))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))

	var parsed struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body := w.Body.String()
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("response is not a single JSON document: %v; body=%q", err, body)
	}
	if len(parsed.Data) != 2 || parsed.Data[0].ID != "real-1" || parsed.Data[1].ID != "c1" {
		t.Fatalf("unexpected data: %+v", parsed.Data)
	}
}

func TestCombosAugmentModelsFiltersByAPIKeyPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	combos.SyncFromConfig(&config.Config{Combos: []config.ComboConfig{
		{Name: "leader", Models: []config.ComboModelRef{{Provider: "codex", Model: "gpt-5.6-sol"}}},
	}})
	defer combos.SyncFromConfig(&config.Config{})

	enforcer := apikeypolicy.Default()
	enforcer.Replace([]config.APIKeyPolicy{
		{
			Key:    "test-restricted-key",
			Models: []string{"openagentic/gemini-2.5-flash", "gpt-5.6-sol"},
		},
	})
	defer enforcer.Replace(nil)

	s := &Server{}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if k := c.GetHeader("X-Test-Key"); k != "" {
			c.Set("userApiKey", k)
		}
		c.Next()
	})
	r.GET("/v1/models", s.combosAugmentModels(func(c *gin.Context) {
		c.JSON(200, gin.H{
			"object": "list",
			"data": []gin.H{
				{"id": "gemini-2.5-flash", "owned_by": "openagentic"},
				{"id": "gemini-2.5-flash", "owned_by": "antigravity"},
				{"id": "gpt-5.6-sol", "owned_by": "codex"},
				{"id": "claude-sonnet-4-6", "owned_by": "antigravity"},
			},
		})
	}))

	// 1. Calling with unrestricted key -> sees all models plus namespaced variants and combos
	wUnrestricted := httptest.NewRecorder()
	reqUnrestricted := httptest.NewRequest("GET", "/v1/models", nil)
	r.ServeHTTP(wUnrestricted, reqUnrestricted)

	var parsedUnrestricted struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wUnrestricted.Body.Bytes(), &parsedUnrestricted)
	if len(parsedUnrestricted.Data) < 4 {
		t.Fatalf("expected all models, got %d", len(parsedUnrestricted.Data))
	}

	// 2. Calling with restricted key -> sees ONLY openagentic gemini and gpt-5.6-sol
	wRestricted := httptest.NewRecorder()
	reqRestricted := httptest.NewRequest("GET", "/v1/models", nil)
	reqRestricted.Header.Set("X-Test-Key", "test-restricted-key")
	r.ServeHTTP(wRestricted, reqRestricted)

	var parsedRestricted struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wRestricted.Body.Bytes(), &parsedRestricted)

	ids := make([]string, 0, len(parsedRestricted.Data))
	for _, d := range parsedRestricted.Data {
		ids = append(ids, d.ID)
		if strings.Contains(d.ID, "antigravity") || strings.Contains(d.ID, "claude") {
			t.Fatalf("restricted key saw disallowed model: %s (%s)", d.ID, d.OwnedBy)
		}
	}

	// Verify openagentic/gemini-2.5-flash and gpt-5.6-sol are present
	hasOpenagenticGemini := false
	hasGPT56Sol := false
	for _, id := range ids {
		if id == "openagentic/gemini-2.5-flash" || id == "gemini-2.5-flash" {
			hasOpenagenticGemini = true
		}
		if id == "gpt-5.6-sol" || id == "codex/gpt-5.6-sol" {
			hasGPT56Sol = true
		}
	}
	if !hasOpenagenticGemini {
		t.Fatalf("expected openagentic gemini in restricted models, got: %v", ids)
	}
	if !hasGPT56Sol {
		t.Fatalf("expected gpt-5.6-sol in restricted models, got: %v", ids)
	}
}
func TestCombosChatWrapperFallsBackOnModelUnsupported400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	combos.SyncFromConfig(&config.Config{Combos: []config.ComboConfig{
		{
			Name:     "test-fallback-combo",
			Strategy: config.ComboStrategyFallback,
			Models: []config.ComboModelRef{
				{Provider: "codex", Model: "gpt-6-astra"},
				{Provider: "codex", Model: "gpt-5.6-sol"},
			},
		},
	}})
	defer combos.SyncFromConfig(&config.Config{})

	registry.GetGlobalRegistry().RegisterClient("mock-codex", "codex", []*registry.ModelInfo{
		{ID: "gpt-6-astra"},
		{ID: "gpt-5.6-sol"},
	})
	defer registry.GetGlobalRegistry().UnregisterClient("mock-codex")
	r := gin.New()
	s := &Server{}

	// Mock handler: gpt-6-astra returns 400 model unsupported, gpt-5.6-sol returns 200 OK
	r.POST("/v1/chat/completions", s.combosChatWrapper(func(c *gin.Context) {
		var req struct {
			Model string `json:"model"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "bad json"})
			return
		}
		if req.Model == "gpt-6-astra" {
			c.JSON(400, gin.H{"detail": "The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."})
			return
		}
		if req.Model == "gpt-5.6-sol" {
			c.JSON(200, gin.H{"choices": []gin.H{{"message": gin.H{"content": "fallback succeeded"}}}})
			return
		}
		c.JSON(500, gin.H{"error": "unexpected model"})
	}))

	w := httptest.NewRecorder()
	reqBody := `{"model":"test-fallback-combo","messages":[{"role":"user","content":"hello"}]}`
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))

	if w.Code != 200 {
		t.Fatalf("expected HTTP 200 after fallback, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fallback succeeded") {
		t.Fatalf("expected fallback response, got %s", w.Body.String())
	}
}

func TestCombosContextWindowDefault1M_OpenAI_and_Anthropic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	combos.SyncFromConfig(&config.Config{Combos: []config.ComboConfig{
		{
			Name: "gemini-combo",
			Models: []config.ComboModelRef{
				{Provider: "antigravity", Model: "gemini-3.8-flash-high"},
			},
		},
		{
			Name: "claude-combo",
			Models: []config.ComboModelRef{
				{Provider: "antigravity", Model: "claude-opus-4-6-thinking"},
			},
		},
		{
			Name: "codex-combo",
			Models: []config.ComboModelRef{
				{Provider: "codex", Model: "gpt-6-astra"},
			},
		},
		{
			Name: "custom-2m",
			ContextLength: 2000000,
			MaxTokens: 256000,
			Models: []config.ComboModelRef{
				{Provider: "codex", Model: "gpt-6-astra"},
			},
		},
	}})
	defer combos.SyncFromConfig(&config.Config{})

	s := &Server{}
	r := gin.New()

	// 1. Test OpenAI format: /v1/models
	r.GET("/v1/models", s.combosAugmentModels(func(c *gin.Context) {
		if isAnthropicModelsRequest(c) {
			c.JSON(200, gin.H{
				"data": []gin.H{
					{"id": "claude-sonnet-4-6", "type": "model", "display_name": "Claude 4.6 Sonnet"},
				},
				"first_id": "claude-sonnet-4-6",
				"has_more": false,
			})
			return
		}
		c.JSON(200, gin.H{
			"object": "list",
			"data": []gin.H{
				{"id": "gpt-6-astra", "owned_by": "codex"},
			},
		})
	}))
	r.GET("/v1/models/*model", s.getModelHandler(nil, nil))

	// Verify OpenAI /v1/models response
	{
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp struct {
			Data []struct {
				ID            string `json:"id"`
				ContextLength int    `json:"context_length"`
				MaxTokens     int    `json:"max_tokens"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		var geminiFound, claudeFound, codexFound, customFound bool
		for _, m := range resp.Data {
			if m.ID == "gemini-combo" {
				geminiFound = true
				if m.ContextLength != 1048576 {
					t.Fatalf("expected gemini-combo context_length == 1048576, got %d", m.ContextLength)
				}
			}
			if m.ID == "claude-combo" {
				claudeFound = true
				if m.ContextLength != 1000000 {
					t.Fatalf("expected claude-combo context_length == 1000000, got %d", m.ContextLength)
				}
			}
			if m.ID == "codex-combo" {
				codexFound = true
				if m.ContextLength != 272000 {
					t.Fatalf("expected codex-combo context_length == 272000, got %d", m.ContextLength)
				}
			}
			if m.ID == "custom-2m" {
				customFound = true
				if m.ContextLength != 2000000 {
					t.Fatalf("expected custom-2m context_length == 2000000, got %d", m.ContextLength)
				}
			}
		}
		if !geminiFound || !claudeFound || !codexFound || !customFound {
			t.Fatalf("expected all combos in OpenAI /v1/models")
		}
	}

	// Verify Anthropic format /v1/models response (with Anthropic-Version header)
	{
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp struct {
			Data []struct {
				ID             string `json:"id"`
				Type           string `json:"type"`
				MaxInputTokens int    `json:"max_input_tokens"`
				ContextLength  int    `json:"context_length"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		var claudeComboFound bool
		for _, m := range resp.Data {
			if m.ID == "claude-combo" {
				claudeComboFound = true
				if m.ContextLength != 1000000 || m.MaxInputTokens != 1000000 {
					t.Fatalf("expected claude-combo in Anthropic format to have 1M context, got ctx=%d, max_input=%d", m.ContextLength, m.MaxInputTokens)
				}
			}
		}
		if !claudeComboFound {
			t.Fatalf("expected claude-combo in Anthropic /v1/models response")
		}
	}

	// Verify GET /v1/models/:model endpoint for direct model query
	{
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models/gemini-combo", nil))
		if w.Code != 200 {
			t.Fatalf("expected 200 for /v1/models/gemini-combo, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.ID != "gemini-combo" || resp.ContextLength != 1048576 {
			t.Fatalf("unexpected single model response: %+v", resp)
		}
	}
}

func TestCustomCompatProvidersExposedInModelsList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "agentrouter",
				Models: []config.OpenAICompatibilityModel{
					{Name: "gpt-6-astra"},
					{Name: "claude-opus-4-8"},
					{Name: "deepseek-v4-flash"},
				},
			},
			{
				Name: "dahono",
				Models: []config.OpenAICompatibilityModel{
					{Name: "dahono/deepseek-v4-flash"},
					{Name: "ai-chat"},
				},
			},
		},
	}

	s := &Server{cfg: cfg}
	r := gin.New()
	r.GET("/v1/models", s.combosAugmentModels(func(c *gin.Context) {
		c.JSON(200, gin.H{
			"object": "list",
			"data": []gin.H{
				{"id": "gpt-6-astra", "owned_by": "openai"},
				{"id": "gemini-3.8-flash-high", "owned_by": "antigravity"},
			},
		})
	}))
	r.GET("/v1/models/*model", s.getModelHandler(nil, nil))

	// 1. Verify GET /v1/models contains agentrouter/gpt-6-astra, dahono/ai-chat, etc.
	{
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp struct {
			Data []struct {
				ID      string `json:"id"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)

		ids := make(map[string]string)
		for _, m := range resp.Data {
			ids[m.ID] = m.OwnedBy
		}

		expectedIDs := []string{
			"agentrouter/gpt-6-astra",
			"agentrouter/claude-opus-4-8",
			"agentrouter/deepseek-v4-flash",
			"dahono/deepseek-v4-flash",
			"dahono/ai-chat",
			"ai-chat",
		}
		for _, expected := range expectedIDs {
			if _, ok := ids[expected]; !ok {
				t.Fatalf("expected model %q in /v1/models, but was missing. Available: %v", expected, ids)
			}
		}
	}

	// 2. Verify GET /v1/models/agentrouter/gpt-6-astra works directly
	{
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models/agentrouter/gpt-6-astra", nil))
		if w.Code != 200 {
			t.Fatalf("expected 200 for agentrouter/gpt-6-astra, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.ID != "agentrouter/gpt-6-astra" || resp.OwnedBy != "agentrouter" {
			t.Fatalf("unexpected response: %+v", resp)
		}
	}
}

func TestCombosContextAwareGuard_BestEffortAndSkip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	combos.SyncFromConfig(&config.Config{Combos: []config.ComboConfig{
		{
			Name: "tiered-combo",
			Strategy: "fallback",
			Models: []config.ComboModelRef{
				{Provider: "codex", Model: "gpt-6-astra"},
				{Provider: "antigravity", Model: "gemini-3.8-flash-high"},
			},
		},
		{
			Name: "assistant-specialist-test",
			Strategy: "fallback",
			Models: []config.ComboModelRef{
				{Provider: "antigravity", Model: "gemini-3.8-flash-high"},
				{Provider: "antigravity", Model: "claude-opus-4-6-thinking"},
			},
		},
	}})

	registry.GetGlobalRegistry().RegisterClient("mock-client", "mixed", []*registry.ModelInfo{
		{ID: "gpt-6-astra"},
		{ID: "gemini-3.8-flash-high"},
		{ID: "claude-opus-4-6-thinking"},
	})
	defer registry.GetGlobalRegistry().UnregisterClient("mock-client")

	s := &Server{}
	var attemptedModels []string
	r := gin.New()
	r.POST("/v1/chat/completions", s.combosChatWrapper(func(c *gin.Context) {
		var req struct {
			Model string `json:"model"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "bad json"})
			return
		}
		attemptedModels = append(attemptedModels, req.Model)
		c.JSON(200, gin.H{"choices": []gin.H{{"message": gin.H{"content": "ok"}}}})
	}))

	// 1. Payload of 2MB (~500k tokens at /4). Should skip gpt-6-astra (272k) and execute gemini-3.8-flash-high directly.
	{
		attemptedModels = nil
		padding := strings.Repeat("x", 2*1024*1024)
		body := fmt.Sprintf(`{"model":"tiered-combo","messages":[{"role":"user","content":"%s"}]}`, padding)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

		if w.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if len(attemptedModels) != 1 || attemptedModels[0] != "gemini-3.8-flash-high" {
			t.Fatalf("expected only gemini-3.8-flash-high attempted (skipping 272k model), got: %v", attemptedModels)
		}
	}

	// 2. 3.15 MB payload for assistant-specialist-test (~789k tokens at /4).
	// With the new 4-byte heuristic, this fits within gemini-3.8-flash-high (1048576) and claude-opus-4-6-thinking (1000000).
	{
		attemptedModels = nil
		padding := strings.Repeat("a", 315*1024*1024/100)
		body := fmt.Sprintf(`{"model":"assistant-specialist-test","messages":[{"role":"user","content":"%s"}]}`, padding)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

		if w.Code != 200 {
			t.Fatalf("expected 200 for 3.15MB payload, got %d: %s", w.Code, w.Body.String())
		}
		if len(attemptedModels) != 1 || attemptedModels[0] != "gemini-3.8-flash-high" {
			t.Fatalf("expected gemini-3.8-flash-high to be attempted without false-skip, got: %v", attemptedModels)
		}
	}

	// 3. Huge payload of 5MB (~1.25M tokens). Exceeds all models' context length.
	// Should NOT return 502 "no attemptable members"; instead attempts the largest model as best-effort.
	{
		attemptedModels = nil
		padding := strings.Repeat("z", 5*1024*1024)
		body := fmt.Sprintf(`{"model":"assistant-specialist-test","messages":[{"role":"user","content":"%s"}]}`, padding)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

		if w.Code != 200 {
			t.Fatalf("expected 200 via best-effort, got %d: %s", w.Code, w.Body.String())
		}
		if len(attemptedModels) == 0 {
			t.Fatalf("expected at least one member attempted as best-effort, got none")
		}
	}
}

func newComboTestServer(t *testing.T, combo config.ComboConfig, cfg config.Config, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg.Combos = []config.ComboConfig{combo}
	combos.SyncFromConfig(&cfg)
	t.Cleanup(func() { combos.SyncFromConfig(&config.Config{}) })

	registry.GetGlobalRegistry().RegisterClient("mock-combo", "antigravity", []*registry.ModelInfo{
		{ID: "member-a"}, {ID: "member-b"}, {ID: "member-c"},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("mock-combo") })

	r := gin.New()
	s := &Server{}
	r.POST("/v1/chat/completions", s.combosChatWrapper(handler))
	return r
}

func comboRequest(r *gin.Engine, name string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	body := `{"model":"` + name + `","messages":[{"role":"user","content":"hello"}]}`
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return w
}

func TestComboBreaker_SkipsFailedMemberOnNextRequest(t *testing.T) {
	calls := map[string]int{}
	combo := config.ComboConfig{
		Name:     "breaker-combo",
		Strategy: config.ComboStrategyFallback,
		Models: []config.ComboModelRef{
			{Provider: "antigravity", Model: "member-a"},
			{Provider: "antigravity", Model: "member-b"},
		},
	}
	handler := func(c *gin.Context) {
		var req struct {
			Model string `json:"model"`
		}
		_ = c.ShouldBindJSON(&req)
		calls[req.Model]++
		if req.Model == "member-a" {
			c.JSON(503, gin.H{"error": gin.H{"message": "overloaded"}})
			return
		}
		c.JSON(200, gin.H{"choices": []gin.H{{"message": gin.H{"content": "ok"}}}})
	}
	r := newComboTestServer(t, combo, config.Config{}, handler)

	if w := comboRequest(r, "breaker-combo"); w.Code != 200 {
		t.Fatalf("first request status = %d: %s", w.Code, w.Body.String())
	}
	if calls["member-a"] != 1 || calls["member-b"] != 1 {
		t.Fatalf("first request calls = %v", calls)
	}

	// Second request must skip the cooling member entirely and answer directly.
	if w := comboRequest(r, "breaker-combo"); w.Code != 200 {
		t.Fatalf("second request status = %d: %s", w.Code, w.Body.String())
	}
	if calls["member-a"] != 1 {
		t.Fatalf("cooling member was retried: calls=%v", calls)
	}
	if calls["member-b"] != 2 {
		t.Fatalf("healthy member not used: calls=%v", calls)
	}
}

func TestComboBreaker_AllCoolingStillProbesSoonestMember(t *testing.T) {
	calls := 0
	combo := config.ComboConfig{
		Name:     "all-cooling",
		Strategy: config.ComboStrategyFallback,
		Models: []config.ComboModelRef{
			{Provider: "antigravity", Model: "member-a"},
			{Provider: "antigravity", Model: "member-b"},
		},
	}
	handler := func(c *gin.Context) {
		calls++
		c.JSON(503, gin.H{"error": gin.H{"message": "overloaded"}})
	}
	r := newComboTestServer(t, combo, config.Config{}, handler)

	if w := comboRequest(r, "all-cooling"); w.Code == 200 {
		t.Fatal("expected failure when every member is down")
	}
	firstRound := calls
	if firstRound != 2 {
		t.Fatalf("first request attempts = %d, want 2", firstRound)
	}

	// Both members are cooling now. The best-effort probe must still reach one
	// member instead of returning "no attemptable members", and it must never
	// loop: exactly one extra attempt.
	if w := comboRequest(r, "all-cooling"); w.Code == 200 {
		t.Fatal("expected failure while members are still down")
	}
	if calls != firstRound+1 {
		t.Fatalf("probe attempts = %d, want exactly 1", calls-firstRound)
	}
}

func TestComboFailure_ReturnsShortestRetryAfterAndClampsIt(t *testing.T) {
	combo := config.ComboConfig{
		Name:     "retry-after",
		Strategy: config.ComboStrategyFallback,
		Models: []config.ComboModelRef{
			{Provider: "antigravity", Model: "member-a"},
			{Provider: "antigravity", Model: "member-b"},
		},
	}
	handler := func(c *gin.Context) {
		var req struct {
			Model string `json:"model"`
		}
		_ = c.ShouldBindJSON(&req)
		if req.Model == "member-a" {
			c.JSON(403, gin.H{"error": gin.H{"message": "Verify your account", "retry-after-ms": 86337000}})
			return
		}
		c.Header("Retry-After", "120")
		c.JSON(502, gin.H{"error": gin.H{"message": "bad gateway"}})
	}
	r := newComboTestServer(t, combo, config.Config{}, handler)
	policy := combos.DefaultPolicy()
	policy.RetryAfterCap = 30 * time.Second
	combos.ConfigureHealth(policy)

	w := comboRequest(r, "retry-after")
	if w.Code != 502 {
		t.Fatalf("status = %d, want 502 (shortest wait wins): %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "86337000") {
		t.Fatalf("multi-hour retry hint leaked to the client: %s", w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "" && got != "30" {
		t.Fatalf("Retry-After = %q, want empty or capped 30", got)
	}
}

func TestComboPoisonRequest_DoesNotFallThrough(t *testing.T) {
	calls := 0
	combo := config.ComboConfig{
		Name:     "poison",
		Strategy: config.ComboStrategyFallback,
		Models: []config.ComboModelRef{
			{Provider: "antigravity", Model: "member-a"},
			{Provider: "antigravity", Model: "member-b"},
		},
	}
	handler := func(c *gin.Context) {
		calls++
		c.JSON(400, gin.H{"error": gin.H{"message": "tools.7.custom.input_schema: JSON schema is invalid"}})
	}
	r := newComboTestServer(t, combo, config.Config{}, handler)

	if w := comboRequest(r, "poison"); w.Code != 400 {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Fatalf("poison request attempted %d members, want 1", calls)
	}
}

func TestComboRequestBudget_StopsStartingNewMembers(t *testing.T) {
	calls := 0
	combo := config.ComboConfig{
		Name:     "budget",
		Strategy: config.ComboStrategyFallback,
		Models: []config.ComboModelRef{
			{Provider: "antigravity", Model: "member-a"},
			{Provider: "antigravity", Model: "member-b"},
			{Provider: "antigravity", Model: "member-c"},
		},
	}
	handler := func(c *gin.Context) {
		calls++
		time.Sleep(40 * time.Millisecond)
		c.JSON(503, gin.H{"error": gin.H{"message": "overloaded"}})
	}
	r := newComboTestServer(t, combo, config.Config{}, handler)
	policy := combos.DefaultPolicy()
	policy.RequestBudget = 25 * time.Millisecond
	combos.ConfigureHealth(policy)

	if w := comboRequest(r, "budget"); w.Code == 200 {
		t.Fatal("expected failure")
	}
	if calls != 1 {
		t.Fatalf("attempts = %d, want 1 (budget exhausted after the first member)", calls)
	}
}

func TestReorderVisionFirst_IsStableAndDropsNothing(t *testing.T) {
	chain := []config.ComboModelRef{
		{Provider: "custom", Model: "whisper-1"},
		{Provider: "antigravity", Model: "gemini-3.8-flash-high"},
		{Provider: "codex", Model: "gpt-6-luna"},
	}
	ordered, hasVision := reorderVisionFirst(chain)
	if !hasVision {
		t.Fatal("expected at least one vision-capable member")
	}
	if len(ordered) != len(chain) {
		t.Fatalf("lost members: %d -> %d", len(chain), len(ordered))
	}
	if ordered[len(ordered)-1].Model != "whisper-1" {
		t.Fatalf("non-vision member must sort last, got %+v", ordered)
	}
	// Relative order inside each partition is preserved.
	if ordered[0].Model != "gemini-3.8-flash-high" || ordered[1].Model != "gpt-6-luna" {
		t.Fatalf("unexpected order: %+v", ordered)
	}
}

// A member that asks for a long wait must not pin the client when another member
// failed without any hint (which is retryable immediately).
func TestCombosFailureSelection_PrefersRetryNowOverLongHint(t *testing.T) {
	combo := config.ComboConfig{
		Name:     "hint-combo",
		Strategy: config.ComboStrategyFallback,
		Models: []config.ComboModelRef{
			{Provider: "antigravity", Model: "member-a"},
			{Provider: "antigravity", Model: "member-b"},
		},
	}
	handler := func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		if strings.Contains(string(body), "member-a") {
			c.Header("Retry-After", "3600")
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limited"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream blew up"})
	}
	r := newComboTestServer(t, combo, config.Config{}, handler)
	w := comboRequest(r, "hint-combo")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (hintless failure wins)", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want empty (client may retry now)", got)
	}
}

func TestEstimateComboPromptTokens_IgnoresBase64Media(t *testing.T) {
	blob := strings.Repeat("A", 8000)
	raw := []byte(`{"contents":[{"parts":[{"text":"describe this"},{"inlineData":{"mimeType":"image/png","data":"` + blob + `"}}]}],"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + blob + `"}}]}]}`)
	got := estimateComboPromptTokens(raw)
	if got > 100 {
		t.Fatalf("estimated %d tokens for a short prompt with inline images", got)
	}
	if got == 0 {
		t.Fatal("expected a non-zero estimate for the text part")
	}
}
