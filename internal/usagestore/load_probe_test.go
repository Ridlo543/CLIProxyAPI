package usagestore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadProbeCosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage-2026-08.jsonl")
	content := "{\"id\":1,\"timestamp\":\"2026-08-20T10:00:00Z\",\"provider\":\"codex\",\"model\":\"gpt-4o\",\"input_tokens\":1000,\"output_tokens\":500,\"total_tokens\":1500,\"cost\":0.123,\"failed\":false}\n" +
		"{\"id\":2,\"timestamp\":\"2026-08-25T18:18:14Z\",\"provider\":\"codex\",\"model\":\"gpt-debug\",\"input_tokens\":2000,\"output_tokens\":300,\"total_tokens\":2300,\"cost\":0.456,\"failed\":false}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Configure(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Default().Close() }()
	evs := Default().Query(time.UnixMilli(1), time.Now().Add(time.Hour))
	for _, e := range evs {
		t.Logf("id=%d model=%s cost=%f", e.ID, e.Model, e.Cost)
	}
	if evs[0].Cost != 0.123 {
		t.Fatalf("expected 0.123 got %f", evs[0].Cost)
	}
}
