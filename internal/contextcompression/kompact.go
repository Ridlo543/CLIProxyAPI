package contextcompression

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var defaultHTTPClient = &http.Client{
	Timeout: 2500 * time.Millisecond,
}

// applyKompact forwards the raw OpenAI/Anthropic request body to the Kompact
// compression daemon at /v1/compress. If Kompact is down, unreachable, or returns
// a larger payload, this returns false so the caller safely fails open to raw payload.
func applyKompact(ctx context.Context, raw []byte, kcfg config.KompactConfig, stats *Stats) ([]byte, bool) {
	host := kcfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := kcfg.Port
	if port == 0 {
		port = 7878
	}
	timeout := 1500 * time.Millisecond
	if kcfg.TimeoutMS > 0 {
		timeout = time.Duration(kcfg.TimeoutMS) * time.Millisecond
	}

	url := fmt.Sprintf("http://%s:%d/v1/compress", host, port)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		stats.Reason = "kompact_req_err"
		return raw, false
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := defaultHTTPClient.Do(httpReq)
	if err != nil {
		stats.Reason = "kompact_unreachable"
		return raw, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		stats.Reason = fmt.Sprintf("kompact_http_%d", resp.StatusCode)
		return raw, false
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		stats.Reason = "kompact_read_err"
		return raw, false
	}

	var parsed struct {
		Payload          any     `json:"payload"`
		TokensBefore     int     `json:"tokens_before"`
		TokensAfter      int     `json:"tokens_after"`
		TokensSaved      int     `json:"tokens_saved"`
		CompressionRatio float64 `json:"compression_ratio"`
		LatencyMS        float64 `json:"latency_ms"`
	}

	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.Payload == nil {
		stats.Reason = "kompact_invalid_resp"
		return raw, false
	}

	out, err := json.Marshal(parsed.Payload)
	if err != nil || len(out) >= len(raw) {
		stats.Reason = "not_smaller"
		return raw, false
	}

	stats.Applied = true
	stats.Reason = "applied"
	stats.Compressed = 1
	return out, true
}
