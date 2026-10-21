package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/combos"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestCombosWriter_KeepAliveCommitsWithoutPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	state := &comboCommitState{}
	w := newCombosWriter(c.Writer, state)
	w.Header().Set("Content-Type", "text/event-stream")

	heartbeat := []byte(": keep-alive\n\n")
	if n, err := w.Write(heartbeat); n != len(heartbeat) || err != nil {
		t.Fatalf("heartbeat write = (%d, %v)", n, err)
	}
	if w.Passthrough() {
		t.Fatal("a keep-alive comment must not count as delivered content")
	}
	if !w.Committed() || !state.committed {
		t.Fatal("the heartbeat must commit the response headers")
	}
	w.Flush() // must be safe after commit

	w.WriteHeader(http.StatusGatewayTimeout)
	if got := w.StatusCode(); got != http.StatusGatewayTimeout {
		t.Fatalf("member status = %d, want 504", got)
	}
	_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("a failed member's body must stay held, got %q", rec.Body.String())
	}
}

func TestCombosWriter_HeldPayloadFlushesAfterHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	state := &comboCommitState{}
	w := newCombosWriter(c.Writer, state)
	w.Header().Set("Content-Type", "text/event-stream")

	_, _ = w.Write([]byte(": keep-alive\n\n"))
	w.WriteHeader(http.StatusOK)
	payload := []byte("data: {\"choices\":[]}\n\n")
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("recorder code = %d", rec.Code)
	}
	w.FlushHeld()

	if !w.Passthrough() {
		t.Fatal("accepted member must pass through")
	}
	body := rec.Body.String()
	if !strings.Contains(body, ": keep-alive") || !strings.Contains(body, string(payload)) {
		t.Fatalf("expected heartbeat then payload, got %q", body)
	}
	if !strings.HasSuffix(body, string(payload)) {
		t.Fatalf("payload must be the last write, got %q", body)
	}
}

// TestCombosKeepAliveDoesNotBlockFailover is the regression for the production
// stall: the streaming handler flushes an SSE keep-alive after a few seconds,
// which used to mark the response as passthrough. A member that then failed
// with 504 was reported as "completed" and never cooled down, so every request
// paid another first-token timeout on the same dead member.
func TestCombosKeepAliveDoesNotBlockFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	combos.SyncFromConfig(&config.Config{Combos: []config.ComboConfig{{
		Name:     "keepalive-fallback",
		Strategy: config.ComboStrategyFallback,
		Models: []config.ComboModelRef{
			{Provider: "mock", Model: "model-a"},
			{Provider: "mock", Model: "model-b"},
		},
	}}})
	defer combos.SyncFromConfig(&config.Config{})
	defer combos.ResetHealth("", "")

	registry.GetGlobalRegistry().RegisterClient("mock-models", "mock", []*registry.ModelInfo{
		{ID: "model-a"}, {ID: "model-b"},
	})
	defer registry.GetGlobalRegistry().UnregisterClient("mock-models")

	r := gin.New()
	s := &Server{}
	r.POST("/v1/chat/completions", s.combosChatWrapper(func(c *gin.Context) {
		var req struct {
			Model string `json:"model"`
		}
		_ = c.ShouldBindJSON(&req)
		switch req.Model {
		case "model-a":
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
			c.Writer.Flush()
			time.Sleep(20 * time.Millisecond)
			c.Writer.WriteHeader(http.StatusGatewayTimeout)
			_, _ = c.Writer.Write([]byte(`{"error":{"message":"Request timed out"}}`))
		case "model-b":
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unexpected model"})
		}
	}))

	rec := httptest.NewRecorder()
	body := `{"model":"keepalive-fallback","messages":[{"role":"user","content":"hi"}]}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("expected failover to model-b, got body=%q status=%d", rec.Body.String(), rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Request timed out") {
		t.Fatalf("member-a failure leaked into the response: %q", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("keep-alive already committed 200, got %d", rec.Code)
	}
	cooling := false
	for _, m := range combos.SnapshotHealth(time.Now()) {
		if strings.EqualFold(m.Provider, "mock") && strings.EqualFold(m.Model, "model-a") && m.State == combos.MemberOpen {
			cooling = true
		}
	}
	if !cooling {
		t.Fatalf("model-a must be cooling after its 504, snapshot=%+v", combos.SnapshotHealth(time.Now()))
	}
}
