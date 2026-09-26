// Package contextcompression performs fail-open, inline compression of historical tool output.
package contextcompression

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

// Runtime owns context compression state and lifecycle for one API service.
type Runtime struct{}

func NewRuntime() *Runtime { return &Runtime{} }

// Shutdown is a no-op when external TARE binary execution is removed.
func (r *Runtime) Shutdown(ctx context.Context) error { return nil }

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
	validEngines := map[string]bool{
		"off":             true,
		"rtk":             true,
		"tare_structural": true,
		"rtk_tare":        true,
		"kompact":         true,
		"token_savior":    true,
		"all":             true,
		"pipeline":        true,
	}
	validReasons := map[string]bool{
		"disabled":             true,
		"applied":              true,
		"no_eligible":          true,
		"unavailable":          true,
		"checksum_required":    true,
		"checksum_mismatch":    true,
		"version_mismatch":     true,
		"spawn_error":          true,
		"nonzero":              true,
		"timeout":              true,
		"queue_timeout":        true,
		"aborted":              true,
		"shutdown":             true,
		"input_limit":          true,
		"stdout_limit":         true,
		"stderr_limit":         true,
		"stdin_error":          true,
		"invalid_utf8":         true,
		"not_smaller":          true,
		"unknown":              true,
		"opt_out":              true,
		"invalid_json":         true,
		"kompact_req_err":      true,
		"kompact_unreachable":  true,
		"kompact_read_err":     true,
		"kompact_invalid_resp": true,
		"kompact_fallback_raw": true,
	}
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
	stats.Version = strings.TrimSpace(stats.Version)
	stats.ManifestID = strings.TrimSpace(stats.ManifestID)
	return stats
}

// Apply returns a provider-facing copy. Parse/process failures never alter the original payload.
func (r *Runtime) Apply(ctx context.Context, raw []byte, cfg config.ContextCompressionConfig, optOut bool) ([]byte, Stats) {
	started := time.Now()
	stats := Stats{Engine: cfg.Engine, Reason: "disabled"}
	finish := func(out []byte) ([]byte, Stats) {
		stats.ElapsedMS = time.Since(started).Milliseconds()
		stats.BytesBefore = len(raw)
		stats.BytesAfter = len(out)
		GetGlobalMetrics().Record(stats.Engine, len(raw), len(out), stats.Applied)
		return out, stats
	}
	if optOut {
		stats.Reason = "opt_out"
		return finish(raw)
	}
	if cfg.Engine == "" || cfg.Engine == config.ContextCompressionOff {
		stats.Engine = config.ContextCompressionOff
		return finish(raw)
	}

	// Multi-stage pipeline when engine is "all" or "pipeline"
	if cfg.Engine == config.ContextCompressionAll || cfg.Engine == "pipeline" {
		stats.Engine = "pipeline"
		current := raw
		var stagesApplied int

		// Stage 1: Kompact
		if cfg.Kompact.Enabled {
			var kStats Stats
			kOut, kOk := applyKompact(ctx, current, cfg.Kompact, &kStats)
			if kOk && len(kOut) < len(current) {
				current = kOut
				stagesApplied++
				stats.Compressed++
			}
		}

		// Stage 2: RTK on remaining content
		dec := json.NewDecoder(bytes.NewReader(current))
		dec.UseNumber()
		var body any
		if err := dec.Decode(&body); err == nil {
			slots := collectSlots(body, cfg.RawCapBytes)
			eligible := slots[:0]
			for _, candidate := range slots {
				if len([]byte(candidate.text)) >= cfg.MinBytes {
					eligible = append(eligible, candidate)
				}
			}
			if len(eligible) > 0 {
				var rtkStats Stats
				if applyRTK(eligible, cfg.MinBytes, &rtkStats) && rtkStats.Compressed > 0 {
					if rtkOut, err := json.Marshal(body); err == nil && len(rtkOut) < len(current) {
						current = rtkOut
						stagesApplied++
						stats.Compressed += rtkStats.Compressed
					}
				}
			}
		}

		if stagesApplied > 0 && len(current) < len(raw) {
			stats.Applied = true
			stats.Reason = "applied"
			savedBytes := len(raw) - len(current)
			pct := float64(savedBytes) / float64(len(raw)) * 100.0
			log.Infof("[PIPELINE] ⚡ -%.1f%% (Kompact+RTK: saved %dB, %dB → %dB in %dms)", pct, savedBytes, len(raw), len(current), time.Since(started).Milliseconds())
			return finish(current)
		}
		stats.Reason = "not_smaller"
		return finish(raw)
	}

	if cfg.Engine == config.ContextCompressionKompact {
		stats.Engine = config.ContextCompressionKompact
		out, ok := applyKompact(ctx, raw, cfg.Kompact, &stats)
		if ok && len(out) < len(raw) {
			stats.Applied = true
			stats.Reason = "applied"
			savedBytes := len(raw) - len(out)
			pct := float64(savedBytes) / float64(len(raw)) * 100.0
			log.Infof("[KOMPACT] ⚡ -%.1f%% (saved %dB, %dB → %dB in %dms)", pct, savedBytes, len(raw), len(out), time.Since(started).Milliseconds())
			return finish(out)
		}
		stats.Reason = "kompact_fallback_raw"
		return finish(raw)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body any
	if err := dec.Decode(&body); err != nil {
		stats.Reason = "invalid_json"
		return finish(raw)
	}
	slotCap := cfg.RawCapBytes
	if slotCap <= 0 {
		slotCap = 10 * 1024 * 1024
	}
	slots := collectSlots(body, slotCap)
	eligible := slots[:0]
	for _, candidate := range slots {
		if len([]byte(candidate.text)) >= cfg.MinBytes {
			eligible = append(eligible, candidate)
		}
	}
	slots = eligible
	stats.Selected = len(slots)
	if len(slots) == 0 {
		stats.Reason = "no_eligible"
		return finish(raw)
	}

	ok := applyRTK(slots, cfg.MinBytes, &stats)
	if !ok {
		return finish(raw)
	}
	out, err := json.Marshal(body)
	if err != nil || len(out) >= len(raw) {
		stats.Applied = false
		stats.Reason = "not_smaller"
		return finish(raw)
	}
	stats.Applied = true
	stats.Reason = "applied"
	savedBytes := len(raw) - len(out)
	pct := float64(savedBytes) / float64(len(raw)) * 100.0

	log.Infof("[RTK] ✂️ -%.1f%% (%d slots, saved %dB in %dms)", pct, stats.Compressed, savedBytes, stats.ElapsedMS)
	return finish(out)
}
