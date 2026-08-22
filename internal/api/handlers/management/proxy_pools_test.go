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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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
	var statuses []coreauth.ProxyPoolStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
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
