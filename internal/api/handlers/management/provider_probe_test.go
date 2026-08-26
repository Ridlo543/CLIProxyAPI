package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newProbeTestServer() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{}
	r.POST("/v0/management/provider-probe", h.ProbeProvider)
	return r
}

func postProbe(t *testing.T, r *gin.Engine, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/provider-probe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	return out
}

func TestProbeProviderOK(t *testing.T) {
	upstream := gin.New()
	upstream.GET("/models", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": []gin.H{{"id": "model-a"}, {"id": "model-b"}, {"id": "model-c"}}})
	})
	ts := httptest.NewServer(upstream)
	defer ts.Close()

	r := newProbeTestServer()
	out := postProbe(t, r, `{"kind":"openai-compat","base_url":"`+ts.URL+`","api_key":"k"}`)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", out)
	}
	if count, _ := out["model_count"].(float64); int(count) != 3 {
		t.Fatalf("model_count = %v, want 3", out["model_count"])
	}
}

func TestProbeProviderAnthropicPath(t *testing.T) {
	var hit string
	upstream := gin.New()
	upstream.GET("/v1/models", func(c *gin.Context) {
		hit = c.Request.URL.Path
		if c.Request.Header.Get("x-api-key") == "" || c.Request.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic headers")
		}
		c.JSON(http.StatusOK, gin.H{"data": []gin.H{{"id": "claude-sonnet-4"}}})
	})
	ts := httptest.NewServer(upstream)
	defer ts.Close()

	r := newProbeTestServer()
	out := postProbe(t, r, `{"kind":"anthropic","base_url":"`+ts.URL+`","api_key":"sk-ant"}`)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", out)
	}
	if hit != "/v1/models" {
		t.Fatalf("upstream path = %q, want /v1/models", hit)
	}
}

func TestProbeProviderBadInput(t *testing.T) {
	r := newProbeTestServer()

	if out := postProbe(t, r, `{"kind":"openai-compat","base_url":"ftp://x","api_key":"k"}`); toBool(out["ok"]) {
		t.Fatalf("non-http scheme should not be ok: %v", out)
	}
	if out := postProbe(t, r, `{"kind":"openai-compat","base_url":"https://x","api_key":""}`); toBool(out["ok"]) {
		t.Fatalf("empty api key should not be ok: %v", out)
	}
}

func TestProbeProviderUpstreamRejectsKey(t *testing.T) {
	upstream := gin.New()
	upstream.GET("/models", func(c *gin.Context) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "bad key"})
	})
	ts := httptest.NewServer(upstream)
	defer ts.Close()

	r := newProbeTestServer()
	out := postProbe(t, r, `{"kind":"openai-compat","base_url":"`+ts.URL+`","api_key":"wrong"}`)
	if toBool(out["ok"]) {
		t.Fatalf("401 upstream must not be ok: %v", out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "401") {
		t.Fatalf("expected humanized 401 message, got %v", out["error"])
	}
}

func toBool(v any) bool {
	b, _ := v.(bool)
	return b
}
