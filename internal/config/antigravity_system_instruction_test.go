package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeAntigravitySystemInstruction_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte("  You are ENI.  \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := &Config{}
	cfg.Antigravity.SystemInstructionFile = path
	cfg.Antigravity.SystemInstructionModels = []string{" Gemini-3.8-Flash ", "", "gemini-3.8-flash"}

	cfg.SanitizeAntigravitySystemInstruction()

	if got := cfg.Antigravity.SystemInstruction; got != "You are ENI." {
		t.Fatalf("instruction = %q, want trimmed file contents", got)
	}
	if len(cfg.Antigravity.SystemInstructionModels) != 1 {
		t.Fatalf("models not deduplicated: %#v", cfg.Antigravity.SystemInstructionModels)
	}
	if cfg.Antigravity.SystemInstructionModels[0] != "Gemini-3.8-Flash" {
		t.Fatalf("model = %q, want original casing preserved", cfg.Antigravity.SystemInstructionModels[0])
	}
}

func TestSanitizeAntigravitySystemInstruction_ExpandsEnvVar(t *testing.T) {
	const key = "TEST_ANTIGRAVITY_PERSONA"
	t.Setenv(key, "persona from env")
	cfg := &Config{}
	cfg.Antigravity.SystemInstruction = "$" + key

	cfg.SanitizeAntigravitySystemInstruction()

	if got := cfg.Antigravity.SystemInstruction; got != "persona from env" {
		t.Fatalf("instruction = %q, want env value", got)
	}
}

func TestSanitizeAntigravitySystemInstruction_UnsetEnvYieldsEmpty(t *testing.T) {
	cfg := &Config{}
	cfg.Antigravity.SystemInstruction = "$TEST_ANTIGRAVITY_PERSONA_DEFINITELY_UNSET"

	cfg.SanitizeAntigravitySystemInstruction()

	if cfg.Antigravity.SystemInstruction != "" {
		t.Fatalf("instruction = %q, want empty when env var is unset", cfg.Antigravity.SystemInstruction)
	}
}

func TestSanitizeAntigravitySystemInstruction_FileWinsOverInline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte("from file"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := &Config{}
	cfg.Antigravity.SystemInstruction = "inline text"
	cfg.Antigravity.SystemInstructionFile = path

	cfg.SanitizeAntigravitySystemInstruction()

	if got := cfg.Antigravity.SystemInstruction; got != "from file" {
		t.Fatalf("instruction = %q, want file contents to win", got)
	}
}

func TestSanitizeAntigravitySystemInstruction_MissingFileLeavesEmpty(t *testing.T) {
	cfg := &Config{}
	cfg.Antigravity.SystemInstructionFile = filepath.Join(t.TempDir(), "does-not-exist.txt")

	cfg.SanitizeAntigravitySystemInstruction()

	if cfg.Antigravity.SystemInstruction != "" {
		t.Fatalf("instruction = %q, want empty for a missing file", cfg.Antigravity.SystemInstruction)
	}
}

func TestSanitizeAntigravitySystemInstruction_NilConfigIsNoop(t *testing.T) {
	var cfg *Config
	cfg.SanitizeAntigravitySystemInstruction()
}
