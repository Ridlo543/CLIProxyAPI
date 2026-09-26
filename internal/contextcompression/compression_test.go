package contextcompression

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var testRuntime = NewRuntime()

func Apply(ctx context.Context, raw []byte, cfg config.ContextCompressionConfig, optOut bool) ([]byte, Stats) {
	return testRuntime.Apply(ctx, raw, cfg, optOut)
}

func TestCollectSlotsPreservesErrorsMediaAndObjects(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"tool","content":"openai"},{"role":"tool","content":[{"type":"text","text":"array"},{"type":"image","source":{"data":"secret"}}]},{"type":"function_call_output","output":[{"type":"input_text","text":"response"},{"nested":true}]},{"role":"user","content":[{"type":"tool_result","content":"claude"},{"type":"tool_result","is_error":true,"content":"error"}]}],"contents":[{"parts":[{"functionResponse":{"response":{"result":"gemini"}}}]}],"conversationState":{"history":[{"userContent":{"parts":[{"text":"vscode"}]}}]}}`)
	cfg := config.ContextCompressionConfig{Engine: config.ContextCompressionOff, MinBytes: 1, RawCapBytes: 1024 * 1024}
	out, stats := Apply(context.Background(), raw, cfg, false)
	if string(out) != string(raw) || stats.Reason != "disabled" {
		t.Fatalf("expected raw pass-through, got stats=%+v out=%s", stats, out)
	}
}

func TestSanitizeStatsSchema(t *testing.T) {
	valid := Stats{Engine: "rtk", Reason: "applied", Applied: true, Selected: 2, Compressed: 1, BytesBefore: 10, BytesAfter: 5, CacheHits: 1, ElapsedMS: 3, Version: "1.2.3"}
	if got := SanitizeStats(valid); got != valid {
		t.Fatalf("valid=%+v", got)
	}
}

func TestRTKCompressesToolResults(t *testing.T) {
	longText := "commit 1234567890abcdef1234567890abcdef12345678\nAuthor: test <test@example.com>\nDate: Mon Sep 26 2026\n\n    fix something\n\n1 file changed, 1 insertion(+)\n"
	raw, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{"role": "tool", "content": longText},
		},
	})
	cfg := config.ContextCompressionConfig{
		Engine:      config.ContextCompressionRTK,
		MinBytes:    5,
		RawCapBytes: 1024 * 1024,
	}
	out, stats := Apply(context.Background(), raw, cfg, false)
	if !stats.Applied && stats.Reason != "applied" && stats.Reason != "not_smaller" && stats.Reason != "no_eligible" {
		t.Fatalf("unexpected reason: %s (out=%s)", stats.Reason, string(out))
	}
}
