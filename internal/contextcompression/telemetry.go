package contextcompression

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// SaverMetrics records cumulative context compression statistics.
type SaverMetrics struct {
	mu sync.RWMutex

	TotalRequests   int64 `json:"total_requests"`
	TotalCompressed int64 `json:"total_compressed"`
	TotalBytesRaw   int64 `json:"total_bytes_raw"`
	TotalBytesOut   int64 `json:"total_bytes_out"`
	TotalBytesSaved int64 `json:"total_bytes_saved"`

	ByEngine map[string]*EngineMetric `json:"by_engine"`
}

type EngineMetric struct {
	Requests   int64 `json:"requests"`
	BytesRaw   int64 `json:"bytes_raw"`
	BytesOut   int64 `json:"bytes_out"`
	BytesSaved int64 `json:"bytes_saved"`
}

var globalMetrics = &SaverMetrics{
	ByEngine: make(map[string]*EngineMetric),
}

func GetGlobalMetrics() *SaverMetrics {
	return globalMetrics
}

func (m *SaverMetrics) Record(engine string, rawBytes, outBytes int, applied bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.TotalRequests++
	m.TotalBytesRaw += int64(rawBytes)
	m.TotalBytesOut += int64(outBytes)

	saved := rawBytes - outBytes
	if saved < 0 {
		saved = 0
	}

	if applied {
		m.TotalCompressed++
		m.TotalBytesSaved += int64(saved)
	}

	em, ok := m.ByEngine[engine]
	if !ok {
		em = &EngineMetric{}
		m.ByEngine[engine] = em
	}
	em.Requests++
	em.BytesRaw += int64(rawBytes)
	em.BytesOut += int64(outBytes)
	if applied {
		em.BytesSaved += int64(saved)
	}
}

// Snapshot returns a copy of current metrics combined with live status from external daemons.
func (m *SaverMetrics) Snapshot(ctx context.Context, cfg config.ContextCompressionConfig) map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	savingsPct := 0.0
	if m.TotalBytesRaw > 0 {
		savingsPct = float64(m.TotalBytesSaved) / float64(m.TotalBytesRaw) * 100.0
	}

	byEngine := make(map[string]map[string]any)
	for k, v := range m.ByEngine {
		engineSavedPct := 0.0
		if v.BytesRaw > 0 {
			engineSavedPct = float64(v.BytesSaved) / float64(v.BytesRaw) * 100.0
		}
		byEngine[k] = map[string]any{
			"requests":    v.Requests,
			"bytes_raw":   v.BytesRaw,
			"bytes_out":   v.BytesOut,
			"bytes_saved": v.BytesSaved,
			"savings_pct": mathRound(engineSavedPct, 1),
		}
	}

	res := map[string]any{
		"engine":               cfg.Engine,
		"total_requests":       m.TotalRequests,
		"total_compressed":     m.TotalCompressed,
		"total_bytes_raw":      m.TotalBytesRaw,
		"total_bytes_out":      m.TotalBytesOut,
		"total_bytes_saved":    m.TotalBytesSaved,
		"savings_pct":          mathRound(savingsPct, 1),
		"estimated_tokens_sav": m.TotalBytesSaved / 4,
		"by_engine":            byEngine,
		"daemons":              queryDaemons(ctx, cfg),
	}
	return res
}

func mathRound(val float64, decimals int) float64 {
	pow := 1.0
	for i := 0; i < decimals; i++ {
		pow *= 10.0
	}
	return float64(int64(val*pow+0.5)) / pow
}

func queryDaemons(ctx context.Context, cfg config.ContextCompressionConfig) map[string]any {
	daemons := make(map[string]any)

	// Check Kompact
	kompHost := cfg.Kompact.Host
	if kompHost == "" {
		kompHost = "127.0.0.1"
	}
	kompPort := cfg.Kompact.Port
	if kompPort == 0 {
		kompPort = 7878
	}
	daemons["kompact"] = queryKompact(ctx, fmt.Sprintf("http://%s:%d/api/metrics", kompHost, kompPort))

	// Check Token Savior
	tsHost := cfg.TokenSavior.Host
	if tsHost == "" {
		tsHost = "127.0.0.1"
	}
	tsPort := cfg.TokenSavior.Port
	if tsPort == 0 {
		tsPort = 8921
	}
	daemons["token_savior"] = queryTokenSavior(ctx, fmt.Sprintf("http://%s:%d/", tsHost, tsPort))

	return daemons
}

func queryKompact(ctx context.Context, url string) map[string]any {
	reqCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return map[string]any{"status": "down"}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return map[string]any{"status": fmt.Sprintf("http_%d", resp.StatusCode)}
	}

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return map[string]any{"status": "invalid_response"}
	}
	data["status"] = "online"
	return data
}

func queryTokenSavior(ctx context.Context, url string) map[string]any {
	reqCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return map[string]any{"status": "down"}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return map[string]any{"status": "read_error"}
	}

	if resp.StatusCode == http.StatusOK && len(body) > 0 {
		return map[string]any{
			"status": "online",
			"type":   "dashboard",
		}
	}
	return map[string]any{"status": fmt.Sprintf("http_%d", resp.StatusCode)}
}
