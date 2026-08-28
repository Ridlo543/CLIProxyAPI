package management

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestore"
)

type clientKeyAgg struct {
	key        string
	name       string
	maskedKey  string
	calls      int64
	success    int64
	failed     int64
	input      int64
	output     int64
	total      int64
	latencies  []int64
	ttfts      []int64
	tpsList    []float64
	lastUsedMs int64
}

func maskClientKey(key string) string {
	k := strings.TrimSpace(key)
	if len(k) <= 12 {
		return k
	}
	prefix := k[:8]
	suffix := k[len(k)-4:]
	return prefix + "••••••••••••" + suffix
}

// GetClientAPIKeyUsage aggregates usage statistics per Client API Key
// currently configured in Endpoint & Keys (api-keys / key-policies).
func (h *Handler) GetClientAPIKeyUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	h.mu.Lock()
	configuredKeys := make([]string, 0, len(h.cfg.APIKeys))
	keyNames := make(map[string]string)

	for _, p := range h.cfg.KeyPolicies {
		k := p.EffectiveKey()
		if k != "" {
			if strings.TrimSpace(p.Name) != "" {
				keyNames[k] = strings.TrimSpace(p.Name)
			}
		}
	}
	for _, p := range h.cfg.APIKeyPolicies {
		k := p.EffectiveKey()
		if k != "" {
			if strings.TrimSpace(p.Name) != "" {
				keyNames[k] = strings.TrimSpace(p.Name)
			}
		}
	}

	for _, rawKey := range h.cfg.APIKeys {
		k := strings.TrimSpace(rawKey)
		if k != "" {
			configuredKeys = append(configuredKeys, k)
		}
	}
	// Also add any keys defined in policies if not in api-keys list
	for k := range keyNames {
		found := false
		for _, existing := range configuredKeys {
			if existing == k {
				found = true
				break
			}
		}
		if !found {
			configuredKeys = append(configuredKeys, k)
		}
	}
	h.mu.Unlock()

	now := time.Now()
	from := now.Add(-30 * 24 * time.Hour)
	to := now

	events := usagestore.Default().Query(from, to)

	index := make(map[string]*clientKeyAgg)

	// ONLY initialize and include currently configured keys (no obsolete/deleted keys)
	for _, k := range configuredKeys {
		name := keyNames[k]
		if name == "" {
			name = "client-key"
		}
		index[k] = &clientKeyAgg{
			key:       k,
			name:      name,
			maskedKey: maskClientKey(k),
		}
	}

	// Aggregate events ONLY for valid configured keys
	for _, ev := range events {
		k := strings.TrimSpace(ev.APIKey)
		if k == "" {
			continue
		}
		a, ok := index[k]
		if !ok {
			// Skip deleted / obsolete keys that are no longer in config
			continue
		}
		a.calls++
		if ev.Failed || ev.StatusCod >= 400 {
			a.failed++
		} else {
			a.success++
		}
		a.input += ev.Input
		a.output += ev.Output
		a.total += ev.Total
		if ev.LatencyMs > 0 {
			a.latencies = append(a.latencies, ev.LatencyMs)
		}
		if ev.TTFTMs > 0 {
			a.ttfts = append(a.ttfts, ev.TTFTMs)
		}
		if ev.Output > 0 && ev.LatencyMs > 0 {
			tps := (float64(ev.Output) / float64(ev.LatencyMs)) * 1000.0
			a.tpsList = append(a.tpsList, tps)
		}
		evMs := ev.Timestamp.UnixMilli()
		if evMs > a.lastUsedMs {
			a.lastUsedMs = evMs
		}
	}

	type ClientKeyUsageItem struct {
		Key           string  `json:"key"`
		Name          string  `json:"name"`
		MaskedKey     string  `json:"masked_key"`
		Calls         int64   `json:"calls"`
		Success       int64   `json:"success"`
		Failed        int64   `json:"failed"`
		SuccessRate   float64 `json:"success_rate"`
		InputTokens   int64   `json:"input_tokens"`
		OutputTokens  int64   `json:"output_tokens"`
		TotalTokens   int64   `json:"total_tokens"`
		AvgLatencyMs  int64   `json:"avg_latency_ms"`
		AvgTTFTMs     int64   `json:"avg_ttft_ms"`
		DiffLatencyMs int64   `json:"diff_latency_ms"`
		AvgSpeedTPS   float64 `json:"avg_speed_tps"`
		LastUsedMs    int64   `json:"last_used_ms"`
	}

	result := make([]ClientKeyUsageItem, 0, len(index))
	for _, a := range index {
		successRate := 1.0
		if a.calls > 0 {
			successRate = float64(a.success) / float64(a.calls)
		}
		avgLatency := int64(0)
		if len(a.latencies) > 0 {
			avgLatency = int64(average(a.latencies))
		}
		avgTTFT := int64(0)
		if len(a.ttfts) > 0 {
			avgTTFT = int64(average(a.ttfts))
		}
		diffLatency := int64(0)
		if avgLatency > avgTTFT && avgTTFT > 0 {
			diffLatency = avgLatency - avgTTFT
		}
		avgSpeed := 0.0
		if len(a.tpsList) > 0 {
			avgSpeed = round1(averageFloat(a.tpsList))
		}

		result = append(result, ClientKeyUsageItem{
			Key:           a.key,
			Name:          a.name,
			MaskedKey:     a.maskedKey,
			Calls:         a.calls,
			Success:       a.success,
			Failed:        a.failed,
			SuccessRate:   successRate,
			InputTokens:   a.input,
			OutputTokens:  a.output,
			TotalTokens:   a.total,
			AvgLatencyMs:  avgLatency,
			AvgTTFTMs:     avgTTFT,
			DiffLatencyMs: diffLatency,
			AvgSpeedTPS:   avgSpeed,
			LastUsedMs:    a.lastUsedMs,
		})
	}

	// Sort by total calls descending
	sort.Slice(result, func(i, j int) bool {
		if result[i].Calls == result[j].Calls {
			return result[i].TotalTokens > result[j].TotalTokens
		}
		return result[i].Calls > result[j].Calls
	})

	c.JSON(http.StatusOK, gin.H{"client_keys": result})
}
