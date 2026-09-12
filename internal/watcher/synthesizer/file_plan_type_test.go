package synthesizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// A stored plan_type used to be lifted into the auth attributes only for codex,
// so an antigravity credential could carry a tier on disk that the management
// API never reported and no panel surface could show.

func synthesizeOne(t *testing.T, fileName string, authData map[string]any) map[string]string {
	t.Helper()
	dir := t.TempDir()
	data, errMarshal := json.Marshal(authData)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, fileName), data, 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	auths, errSynth := NewFileSynthesizer().Synthesize(&SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     dir,
		Now:         time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynth != nil {
		t.Fatalf("Synthesize: %v", errSynth)
	}
	if len(auths) != 1 {
		t.Fatalf("auths = %d, want 1", len(auths))
	}
	return auths[0].Attributes
}

func TestSynthesizeLiftsStoredPlanTypeForAntigravity(t *testing.T) {
	attrs := synthesizeOne(t, "antigravity-auth.json", map[string]any{
		"type":       "antigravity",
		"email":      "pro@example.com",
		"plan_type":  "Google AI Pro",
		"project_id": "proj-1",
	})
	if got := attrs["plan_type"]; got != "Google AI Pro" {
		t.Fatalf("plan_type = %q, want the value stored in the file", got)
	}
}

func TestSynthesizeStillLiftsStoredPlanTypeForCodex(t *testing.T) {
	attrs := synthesizeOne(t, "codex-auth.json", map[string]any{
		"type":      "codex",
		"email":     "plus@example.com",
		"plan_type": "pro",
	})
	if got := attrs["plan_type"]; got != "pro" {
		t.Fatalf("plan_type = %q, want pro", got)
	}
}

func TestSynthesizeInventsNoPlanTypeWhenTheFileHasNone(t *testing.T) {
	attrs := synthesizeOne(t, "antigravity-bare.json", map[string]any{
		"type":  "antigravity",
		"email": "unknown@example.com",
	})
	if got, ok := attrs["plan_type"]; ok {
		t.Fatalf("plan_type = %q, want absent when the file states no tier", got)
	}
}
