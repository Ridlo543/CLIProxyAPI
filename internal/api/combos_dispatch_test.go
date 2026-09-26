package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/combos"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
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
