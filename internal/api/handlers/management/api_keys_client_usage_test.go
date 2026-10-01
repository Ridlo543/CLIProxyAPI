package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestGetClientAPIKeyUsage_ReturnsAggregatedKeys(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	_ = usagestore.Configure(tmpDir)
	defer func() { _ = usagestore.Default().Close() }()

	now := time.Now()
	usagestore.Default().Add(usagestore.RecordFromUsage(coreusage.Record{
		Model:       "gpt-5.6-sol",
		APIKey:      "sk-test-client-key-1",
		RequestedAt: now,
		Latency:     500 * time.Millisecond,
		TTFT:        150 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  1000,
			OutputTokens: 200,
			TotalTokens:  1200,
		},
	}, 200))

	cfg := &config.Config{
		AuthDir: tmpDir,
		SDKConfig: config.SDKConfig{
			APIKeys: []string{"sk-test-client-key-1", "sk-configured-unused-key"},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/api-keys/usage", nil)
	ginCtx.Request = req

	h.GetClientAPIKeyUsage(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		ClientKeys []struct {
			Key           string `json:"key"`
			Calls         int64  `json:"calls"`
			Success       int64  `json:"success"`
			TotalTokens   int64  `json:"total_tokens"`
			AvgLatencyMs  int64  `json:"avg_latency_ms"`
			AvgTTFTMs     int64  `json:"avg_ttft_ms"`
			DiffLatencyMs int64  `json:"diff_latency_ms"`
		} `json:"client_keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(payload.ClientKeys) < 2 {
		t.Fatalf("expected at least 2 client keys, got %d", len(payload.ClientKeys))
	}

	var foundActive bool
	for _, ck := range payload.ClientKeys {
		if ck.Key == "sk-test-client-key-1" {
			foundActive = true
			if ck.Calls != 1 {
				t.Errorf("expected 1 call, got %d", ck.Calls)
			}
			if ck.TotalTokens != 1200 {
				t.Errorf("expected 1200 tokens, got %d", ck.TotalTokens)
			}
			if ck.AvgLatencyMs != 500 {
				t.Errorf("expected 500ms latency, got %d", ck.AvgLatencyMs)
			}
			if ck.AvgTTFTMs != 150 {
				t.Errorf("expected 150ms ttft, got %d", ck.AvgTTFTMs)
			}
			if ck.DiffLatencyMs != 350 {
				t.Errorf("expected 350ms diff latency, got %d", ck.DiffLatencyMs)
			}
		}
	}
	if !foundActive {
		t.Fatalf("active key sk-test-client-key-1 not found in response")
	}
}

func TestGetClientAPIKeyUsage_TimeframeFiltering(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	_ = usagestore.Configure(tmpDir)
	defer func() { _ = usagestore.Default().Close() }()

	now := time.Now()
	// Event 1: 10 minutes ago (within today & 24h)
	usagestore.Default().Add(usagestore.RecordFromUsage(coreusage.Record{
		Model:       "gpt-5.6-sol",
		APIKey:      "sk-timeframe-test-key",
		RequestedAt: now.Add(-10 * time.Minute),
		Latency:     200 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	}, 200))

	// Event 2: 2 days ago (outside 24h, inside 7d)
	usagestore.Default().Add(usagestore.RecordFromUsage(coreusage.Record{
		Model:       "gpt-5.6-sol",
		APIKey:      "sk-timeframe-test-key",
		RequestedAt: now.Add(-48 * time.Hour),
		Latency:     300 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  200,
			OutputTokens: 100,
			TotalTokens:  300,
		},
	}, 200))

	cfg := &config.Config{
		AuthDir: tmpDir,
		SDKConfig: config.SDKConfig{
			APIKeys: []string{"sk-timeframe-test-key"},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	// 1. Query with ?range=24h -> Should only see Event 1 (150 tokens, 1 call)
	{
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys/usage?range=24h", nil)
		h.GetClientAPIKeyUsage(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rec.Code)
		}
		var payload struct {
			ClientKeys []struct {
				Key         string `json:"key"`
				Calls       int64  `json:"calls"`
				TotalTokens int64  `json:"total_tokens"`
			} `json:"client_keys"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
		var targetKeyFound bool
		for _, ck := range payload.ClientKeys {
			if ck.Key == "sk-timeframe-test-key" {
				targetKeyFound = true
				if ck.Calls != 1 {
					t.Fatalf("expected 1 call in 24h, got %d", ck.Calls)
				}
				if ck.TotalTokens != 150 {
					t.Fatalf("expected 150 tokens in 24h, got %d", ck.TotalTokens)
				}
			}
		}
		if !targetKeyFound {
			t.Fatalf("sk-timeframe-test-key not found in 24h response")
		}
	}

	// 2. Query with ?range=7d -> Should see BOTH Event 1 and Event 2 (450 tokens, 2 calls)
	{
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys/usage?range=7d", nil)
		h.GetClientAPIKeyUsage(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rec.Code)
		}
		var payload struct {
			ClientKeys []struct {
				Key         string `json:"key"`
				Calls       int64  `json:"calls"`
				TotalTokens int64  `json:"total_tokens"`
			} `json:"client_keys"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
		var targetKeyFound bool
		for _, ck := range payload.ClientKeys {
			if ck.Key == "sk-timeframe-test-key" {
				targetKeyFound = true
				if ck.Calls != 2 {
					t.Fatalf("expected 2 calls in 7d, got %d", ck.Calls)
				}
				if ck.TotalTokens != 450 {
					t.Fatalf("expected 450 tokens in 7d, got %d", ck.TotalTokens)
				}
			}
		}
		if !targetKeyFound {
			t.Fatalf("sk-timeframe-test-key not found in 7d response")
		}
	}
}
