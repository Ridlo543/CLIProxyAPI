package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContextCompressionValidation(t *testing.T) {
	base := ContextCompressionConfig{Engine: ContextCompressionOff, MinBytes: 500, RawCapBytes: 1024 * 1024}
	if err := base.Validate(); err != nil {
		t.Fatalf("off rejected: %v", err)
	}
	base.Engine = ContextCompressionRTK
	if err := base.Validate(); err != nil {
		t.Fatalf("rtk rejected: %v", err)
	}
	base.Engine = ContextCompressionTokenSavior
	if err := base.Validate(); err != nil {
		t.Fatalf("token_savior rejected: %v", err)
	}
	base.Engine = ContextCompressionKompact
	if err := base.Validate(); err != nil {
		t.Fatalf("kompact rejected: %v", err)
	}
	base.Engine = ContextCompressionAll
	if err := base.Validate(); err != nil {
		t.Fatalf("all rejected: %v", err)
	}
}

func TestContextCompressionDefaults(t *testing.T) {
	cfg := ContextCompressionConfig{Engine: "tare_structural"}
	cfg.applyDefaults()
	if cfg.Engine != ContextCompressionRTK {
		t.Fatalf("expected tare_structural to map to rtk, got %s", cfg.Engine)
	}

	cfgOff := ContextCompressionConfig{Engine: "false"}
	cfgOff.applyDefaults()
	if cfgOff.Engine != ContextCompressionOff {
		t.Fatalf("expected false to map to off, got %s", cfgOff.Engine)
	}
}

func loadCompressionConfig(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := LoadConfig(writeCompressionConfig(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeCompressionConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
