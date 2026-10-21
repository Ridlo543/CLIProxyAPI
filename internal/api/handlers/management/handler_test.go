package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
)

// The failed-attempt ban protects publicly reachable management endpoints. The
// operator's own loopback/private address is intentionally exempt: a few typos
// on the local panel must never lock the operator out of their own router
// (30-minute self-ban incident). Both halves are asserted so the exemption cannot
// silently disable the ban for real remote clients.
func TestAuthenticateManagementKey_IPBanPolicy(t *testing.T) {
	newHandler := func() *Handler {
		return &Handler{
			cfg:                 &config.Config{},
			failedAttempts:      make(map[string]*attemptInfo),
			envSecret:           "test-secret",
			allowRemoteOverride: true,
		}
	}

	t.Run("loopback is exempt from the ban", func(t *testing.T) {
		h := newHandler()
		for i := 0; i < 5; i++ {
			allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "wrong-secret")
			if allowed {
				t.Fatalf("wrong key accepted at attempt %d", i+1)
			}
			if statusCode != http.StatusUnauthorized || errMsg != "invalid management key" {
				t.Fatalf("unexpected auth failure at attempt %d: status=%d msg=%q", i+1, statusCode, errMsg)
			}
		}
		allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret")
		if !allowed {
			t.Fatalf("loopback must stay usable after failed attempts: status=%d msg=%q", statusCode, errMsg)
		}
	})

	t.Run("public client is banned even with the correct key", func(t *testing.T) {
		h := newHandler()
		const remoteIP = "203.0.113.9"
		for i := 0; i < 5; i++ {
			if allowed, _, _ := h.AuthenticateManagementKey(remoteIP, false, "wrong-secret"); allowed {
				t.Fatalf("wrong key accepted at attempt %d", i+1)
			}
		}
		allowed, statusCode, errMsg := h.AuthenticateManagementKey(remoteIP, false, "test-secret")
		if allowed {
			t.Fatal("expected correct key to be denied while banned")
		}
		if statusCode != http.StatusForbidden {
			t.Fatalf("expected forbidden status while banned, got %d", statusCode)
		}
		if !strings.HasPrefix(errMsg, "IP banned due to too many failed attempts. Try again in") {
			t.Fatalf("unexpected banned message: %q", errMsg)
		}
	})

	t.Run("unknown client IP is not treated as local", func(t *testing.T) {
		h := newHandler()
		for i := 0; i < 5; i++ {
			h.AuthenticateManagementKey("", false, "wrong-secret")
		}
		if allowed, statusCode, _ := h.AuthenticateManagementKey("", false, "test-secret"); allowed || statusCode != http.StatusForbidden {
			t.Fatalf("empty client IP must be bannable: allowed=%v status=%d", allowed, statusCode)
		}
	})
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {

	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}
	middleware := h.Middleware()

	t.Run("invalid key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		c.Request.RemoteAddr = "127.0.0.1:12345"
		c.Request.Header.Set("X-Management-Key", "wrong-secret")

		middleware(c)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})

	t.Run("valid key", func(t *testing.T) {
		engine := gin.New()
		engine.GET("/v0/management/config", middleware, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})
}
