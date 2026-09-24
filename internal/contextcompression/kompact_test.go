package contextcompression

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestKompactIntegration(t *testing.T) {
	// Mock Kompact /v1/compress server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/compress" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)

		// Mock reduced response
		resp := map[string]any{
			"payload": map[string]any{
				"model": "gpt-4o",
				"messages": []map[string]any{
					{"role": "user", "content": "compressed"},
				},
			},
			"tokens_before":     100,
			"tokens_after":      40,
			"tokens_saved":      60,
			"compression_ratio": 0.4,
			"latency_ms":        2.5,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	runtime := NewRuntime()
	defer runtime.Shutdown(context.Background())

	rawPayload := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"this is a very long text that needs compression"}]}`)

	u, _ := url.Parse(mockServer.URL)
	port, _ := strconv.Atoi(u.Port())

	kcfg := config.KompactConfig{
		Enabled:   true,
		Host:      "127.0.0.1",
		Port:      port,
		TimeoutMS: 2000,
	}

	cfg := config.ContextCompressionConfig{
		Engine:  config.ContextCompressionKompact,
		Kompact: kcfg,
	}

	out, stats := runtime.Apply(context.Background(), rawPayload, cfg, false)

	if !stats.Applied {
		t.Fatalf("expected Kompact compression applied, got reason=%s", stats.Reason)
	}
	if len(out) >= len(rawPayload) {
		t.Fatalf("expected smaller payload, before=%d after=%d", len(rawPayload), len(out))
	}
}
