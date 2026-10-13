package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type managementPoolReporter struct {
	statuses []coreauth.ProxyPoolStatus
	rt       http.RoundTripper
}

func (p managementPoolReporter) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return p.rt }
func (p managementPoolReporter) ProxyPoolStatuses() []coreauth.ProxyPoolStatus    { return p.statuses }

type managementFailingTransport struct{}

func (managementFailingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("selected proxy pool has no healthy entries")
}

func TestGetProxyPoolsAuthenticationAndSanitization(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRoundTripperProvider(managementPoolReporter{statuses: []coreauth.ProxyPoolStatus{
		{Name: "zeta", Strategy: "random", Strict: true, EntryCount: 1, URLs: []string{"direct"}, Healthy: 1},
		{Name: "alpha", Strategy: "round-robin", EntryCount: 1, URLs: []string{"http://user:password@proxy.example:8080/private?query=secret"}, Healthy: 1},
	}})
	h := &Handler{cfg: &config.Config{}, authManager: manager, failedAttempts: make(map[string]*attemptInfo), envSecret: "secret"}
	router := gin.New()
	router.GET("/proxy-pools", h.Middleware(), h.GetProxyPools)

	for _, tc := range []struct {
		name, key string
		want      int
	}{{"missing", "", http.StatusUnauthorized}, {"invalid", "wrong", http.StatusUnauthorized}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/proxy-pools", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			if tc.key != "" {
				req.Header.Set("X-Management-Key", tc.key)
			}
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/proxy-pools", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Management-Key", "secret")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"user", "password", "/private", "query="} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked %q: %s", secret, body)
		}
	}
	// The management API wraps list responses in a config-keyed envelope,
	// the same shape as auth-files and openai-compatibility.
	var payload struct {
		Pools []coreauth.ProxyPoolStatus `json:"proxy-pools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	statuses := payload.Pools
	if len(statuses) != 2 || statuses[0].Name != "alpha" || statuses[1].URLs[0] != "direct" {
		t.Fatalf("statuses=%#v", statuses)
	}
}

func TestAPICallStrictPoolExhaustionIsGeneric(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRoundTripperProvider(managementPoolReporter{rt: managementFailingTransport{}})
	auth := &coreauth.Auth{ID: "pool-auth", Index: "pool-index", Provider: "gemini", ProxyPool: "office", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/api-call", h.APICall)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api-call", strings.NewReader(`{"auth_index":"pool-index","method":"GET","url":"https://upstream.example"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || rec.Body.String() != "{\"error\":\"request failed\"}" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProxyPoolsCRUDLifecycle(t *testing.T) {
	cfg := &config.Config{}
	h := &Handler{cfg: cfg}
	router := gin.New()
	router.POST("/proxy-pools", h.PostProxyPool)
	router.PUT("/proxy-pools/:name", h.PutProxyPool)
	router.DELETE("/proxy-pools/:name", h.DeleteProxyPool)

	// 1. Create a pool
	createPayload := `{"name":"pool-a","strategy":"round-robin","strict":false,"cooldown":"30s","entries":["direct","http://127.0.0.1:8080"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/proxy-pools", strings.NewReader(createPayload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(cfg.ProxyPools) != 1 || cfg.ProxyPools[0].Name != "pool-a" {
		t.Fatalf("cfg.ProxyPools = %#v", cfg.ProxyPools)
	}

	// 2. Update pool
	updatePayload := `{"strategy":"random","strict":true,"cooldown":"1m"}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/proxy-pools/pool-a", strings.NewReader(updatePayload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cfg.ProxyPools[0].Strategy != "random" || !cfg.ProxyPools[0].Strict || cfg.ProxyPools[0].Cooldown != "1m" {
		t.Fatalf("updated pool = %#v", cfg.ProxyPools[0])
	}

	// 3. Delete pool
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/proxy-pools/pool-a", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(cfg.ProxyPools) != 0 {
		t.Fatalf("expected 0 pools after delete, got %d", len(cfg.ProxyPools))
	}
}

func TestProxyPoolsVerifyDirect(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	router := gin.New()
	router.POST("/proxy-pools/verify", h.VerifyProxy)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/proxy-pools/verify", strings.NewReader(`{"url":"direct"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestProxyPoolsAssign(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "test-auth.json", FileName: "test-auth.json", Provider: "antigravity", Status: coreauth.StatusActive}
	_, _ = manager.Register(context.Background(), auth)

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ProxyPools: []config.ProxyPool{{
				Name:     "webshare",
				Strategy: "round-robin",
				Entries:  []config.ProxyPoolEntry{{URL: "direct"}},
			}},
		},
		GeminiKey: []config.GeminiKey{{APIKey: "k1"}},
	}
	h := &Handler{cfg: cfg, authManager: manager}
	router := gin.New()
	router.POST("/proxy-pools/assign", h.AssignProxyPool)

	// Assign to provider antigravity
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/proxy-pools/assign", strings.NewReader(`{"pool":"webshare","provider":"antigravity"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	updated, _ := manager.GetByID("test-auth.json")
	if updated == nil || updated.ProxyPool != "webshare" {
		t.Fatalf("updated.ProxyPool = %v, want webshare", updated)
	}

	// Toggle back to direct (none)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/proxy-pools/assign", strings.NewReader(`{"pool":"direct","provider":"antigravity"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	updated, _ = manager.GetByID("test-auth.json")
	if updated == nil || updated.ProxyPool != "" {
		t.Fatalf("updated.ProxyPool = %v, want empty (direct)", updated)
	}
}
