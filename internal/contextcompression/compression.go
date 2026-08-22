// Package contextcompression performs fail-open, inline compression of historical tool output.
package contextcompression

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// Runtime owns TARE identity, cache, queue, and child process lifecycle for one API service.
type Runtime struct {
	tare *tareEngine
}

func NewRuntime() *Runtime { return &Runtime{tare: newTAREEngine()} }

// Shutdown rejects subsequent submissions and drains this runtime only.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || r.tare == nil {
		return nil
	}
	return r.tare.shutdown(ctx)
}

const OptOutHeader = "x-9router-token-saver"

type Stats struct {
	Engine      string `json:"engine"`
	Applied     bool   `json:"applied"`
	Reason      string `json:"reason"`
	Selected    int    `json:"selected"`
	Compressed  int    `json:"compressed,omitempty"`
	BytesBefore int    `json:"bytes_before,omitempty"`
	BytesAfter  int    `json:"bytes_after,omitempty"`
	CacheHits   int    `json:"cache_hits,omitempty"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	Version     string `json:"version,omitempty"`
	ManifestID  string `json:"manifest_id,omitempty"`
}

// SanitizeStats enforces the telemetry boundary even for alternate engine implementations.
func SanitizeStats(stats Stats) Stats {
	validEngines := map[string]bool{"off": true, "rtk": true, "tare_structural": true}
	validReasons := map[string]bool{"disabled": true, "applied": true, "no_eligible": true, "unavailable": true, "checksum_required": true, "checksum_mismatch": true, "version_mismatch": true, "spawn_error": true, "nonzero": true, "timeout": true, "queue_timeout": true, "aborted": true, "shutdown": true, "input_limit": true, "stdout_limit": true, "stderr_limit": true, "stdin_error": true, "invalid_utf8": true, "not_smaller": true, "unknown": true, "opt_out": true, "invalid_json": true}
	if !validEngines[stats.Engine] {
		stats.Engine = ""
	}
	if !validReasons[stats.Reason] {
		stats.Reason = ""
	}
	if stats.Selected < 0 {
		stats.Selected = 0
	}
	if stats.Compressed < 0 {
		stats.Compressed = 0
	}
	if stats.BytesBefore < 0 {
		stats.BytesBefore = 0
	}
	if stats.BytesAfter < 0 {
		stats.BytesAfter = 0
	}
	if stats.CacheHits < 0 {
		stats.CacheHits = 0
	}
	if stats.ElapsedMS < 0 || stats.ElapsedMS > 9007199254740991 {
		stats.ElapsedMS = 0
	}
	stats.Version = safeTelemetryVersion(stats.Version)
	stats.ManifestID = safeTelemetryManifest(stats.ManifestID)
	return stats
}

// Apply returns a provider-facing copy. Parse/process failures never alter the original payload.
func (r *Runtime) Apply(ctx context.Context, raw []byte, cfg config.ContextCompressionConfig, optOut bool) ([]byte, Stats) {
	started := time.Now()
	stats := Stats{Engine: cfg.Engine, Reason: "disabled"}
	finish := func() Stats { stats.ElapsedMS = time.Since(started).Milliseconds(); return stats }
	if optOut {
		stats.Reason = "opt_out"
		return raw, finish()
	}
	if cfg.Engine == "" || cfg.Engine == config.ContextCompressionOff {
		stats.Engine = config.ContextCompressionOff
		return raw, finish()
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body any
	if err := dec.Decode(&body); err != nil {
		stats.Reason = "invalid_json"
		return raw, finish()
	}
	slotCap := cfg.RawCapBytes
	if cfg.Engine == config.ContextCompressionTARE || cfg.Engine == "tare-structural" {
		cfg.Engine = config.ContextCompressionTARE
		slotCap = maxTARESlotBytes
		stats.Engine = config.ContextCompressionTARE
	}
	slots := collectSlots(body, slotCap)
	if cfg.Engine == config.ContextCompressionRTK {
		eligible := slots[:0]
		for _, candidate := range slots {
			if len([]byte(candidate.text)) >= cfg.MinBytes {
				eligible = append(eligible, candidate)
			}
		}
		slots = eligible
	}
	stats.Selected = len(slots)
	if len(slots) == 0 {
		stats.Reason = "no_eligible"
		return raw, finish()
	}
	var ok bool
	switch cfg.Engine {
	case config.ContextCompressionRTK:
		ok = applyRTK(slots, cfg.MinBytes, &stats)
	case config.ContextCompressionTARE:
		if r == nil || r.tare == nil {
			stats.Reason = "shutdown"
			return raw, finish()
		}
		ok = r.tare.compress(ctx, slots, cfg.TARE, &stats)
	default:
		stats.Reason = "disabled"
		return raw, finish()
	}
	if !ok {
		return raw, finish()
	}
	out, err := json.Marshal(body)
	if err != nil || len(out) >= len(raw) {
		stats.Applied = false
		stats.Reason = "not_smaller"
		return raw, finish()
	}
	stats.Applied = true
	stats.Reason = "applied"
	return out, finish()
}
