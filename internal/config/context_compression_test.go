package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestContextCompressionValidationFailsClosed(t *testing.T) {
	base := ContextCompressionConfig{Engine: ContextCompressionOff, MinBytes: 500, RawCapBytes: 1024 * 1024, TARE: TAREStructuralConfig{ProcessTimeoutMS: 3000, QueueTimeoutMS: 500, InputLimitBytes: 1024 * 1024, StdoutLimitBytes: 1024 * 1024, StderrLimitBytes: 64 * 1024, GlobalConcurrency: 1, CacheEntries: 128, CacheBytes: 16 * 1024 * 1024}}
	if err := base.Validate(); err != nil {
		t.Fatalf("off rejected: %v", err)
	}
	base.Engine = "auto"
	if err := base.Validate(); err == nil {
		t.Fatal("invalid engine accepted")
	}
	base.Engine = ContextCompressionTARE
	if err := base.Validate(); err == nil {
		t.Fatal("unverified TARE accepted")
	}
	base.Engine = ContextCompressionRTKTARE
	if err := base.Validate(); err == nil {
		t.Fatal("unverified rtk_tare accepted")
	}
}

func TestLoadContextCompressionBundledTAREFallback(t *testing.T) {
	const checksum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	binaryPath := filepath.Join(t.TempDir(), "tare")
	t.Setenv(bundledTAREBinaryEnv, binaryPath)
	t.Setenv(bundledTARESHA256Env, checksum)
	t.Setenv(bundledTAREVersionEnv, "0.2.0")
	t.Setenv(bundledTAREManifestIDEnv, "tare-cli-0.2.0-a8d74e91")

	cfg := loadCompressionConfig(t, "context-compression:\n  engine: tare_structural\n")
	if cfg.ContextCompression.TARE.BinaryPath != binaryPath || cfg.ContextCompression.TARE.SHA256 != checksum ||
		len(cfg.ContextCompression.TARE.AllowedVersions) != 1 || cfg.ContextCompression.TARE.AllowedVersions[0] != "0.2.0" ||
		cfg.ContextCompression.TARE.ManifestID != "tare-cli-0.2.0-a8d74e91" {
		t.Fatalf("bundled identity not applied: %#v", cfg.ContextCompression.TARE)
	}
}

func TestLoadContextCompressionExplicitTAREPrecedence(t *testing.T) {
	const explicitSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	explicitPath := filepath.Join(t.TempDir(), "external-tare")
	t.Setenv(bundledTAREBinaryEnv, filepath.Join(t.TempDir(), "bundled-tare"))
	t.Setenv(bundledTARESHA256Env, strings.Repeat("a", 64))
	t.Setenv(bundledTAREVersionEnv, "0.2.0")
	t.Setenv(bundledTAREManifestIDEnv, "bundled")
	cfg := loadCompressionConfig(t, "context-compression:\n  engine: tare_structural\n  tare-structural:\n    binary-path: \""+strings.ReplaceAll(explicitPath, "\\", "\\\\")+"\"\n    sha256: "+explicitSHA+"\n    allowed-versions: [\"9.0.0\"]\n    manifest-id: external\n")
	if cfg.ContextCompression.TARE.BinaryPath != explicitPath || cfg.ContextCompression.TARE.SHA256 != explicitSHA || cfg.ContextCompression.TARE.ManifestID != "external" {
		t.Fatalf("explicit identity was overridden: %#v", cfg.ContextCompression.TARE)
	}
}

func TestLoadContextCompressionRejectsPartialExplicitAndInvalidBundledIdentity(t *testing.T) {
	t.Setenv(bundledTAREBinaryEnv, "/bundled/tare")
	t.Setenv(bundledTARESHA256Env, strings.Repeat("a", 64))
	t.Setenv(bundledTAREVersionEnv, "0.2.0")
	t.Setenv(bundledTAREManifestIDEnv, "bundled")
	path := writeCompressionConfig(t, "context-compression:\n  engine: tare_structural\n  tare-structural:\n    binary-path: /partial/tare\n")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("partial explicit identity accepted")
	}

	t.Setenv(bundledTARESHA256Env, "invalid")
	path = writeCompressionConfig(t, "context-compression:\n  engine: tare_structural\n")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("invalid bundled identity accepted")
	}
	t.Setenv(bundledTARESHA256Env, "")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("missing bundled identity accepted")
	}
}

func TestLoadContextCompressionOffAndRTKIgnoreBundledEnvironment(t *testing.T) {
	for _, engine := range []string{ContextCompressionOff, ContextCompressionRTK} {
		t.Run(engine, func(t *testing.T) {
			t.Setenv(bundledTAREBinaryEnv, "relative-invalid")
			t.Setenv(bundledTARESHA256Env, "invalid")
			cfg := loadCompressionConfig(t, "context-compression:\n  engine: "+engine+"\n")
			if cfg.ContextCompression.TARE.BinaryPath != "" || cfg.ContextCompression.TARE.SHA256 != "" {
				t.Fatalf("%s consumed bundled identity", engine)
			}
		})
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

func TestContextCompressionNormalizesHyphenAliasToCanonicalUnderscore(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("context-compression:\n  engine: tare-structural\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ContextCompression.applyDefaults()
	if cfg.ContextCompression.Engine != ContextCompressionTARE {
		t.Fatalf("engine=%q", cfg.ContextCompression.Engine)
	}
	out, err := yaml.Marshal(cfg.ContextCompression)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "tare_structural") {
		t.Fatalf("canonical yaml=%s", out)
	}
}

func TestContextCompressionNormalizesCombinedAliasToCanonicalValue(t *testing.T) {
	for _, alias := range []string{"rtk+tare", "rtk_tare"} {
		var cfg Config
		if err := yaml.Unmarshal([]byte("context-compression:\n  engine: "+alias+"\n"), &cfg); err != nil {
			t.Fatal(err)
		}
		cfg.ContextCompression.applyDefaults()
		if cfg.ContextCompression.Engine != ContextCompressionRTKTARE {
			t.Fatalf("alias=%q engine=%q", alias, cfg.ContextCompression.Engine)
		}
		out, err := yaml.Marshal(cfg.ContextCompression)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "rtk_tare") || strings.Contains(string(out), "rtk+tare") {
			t.Fatalf("alias=%q canonical yaml=%s", alias, out)
		}
	}
}

func TestLoadContextCompressionBundledTAREFallbackForCombined(t *testing.T) {
	const checksum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	binaryPath := filepath.Join(t.TempDir(), "tare")
	t.Setenv(bundledTAREBinaryEnv, binaryPath)
	t.Setenv(bundledTARESHA256Env, checksum)
	t.Setenv(bundledTAREVersionEnv, "0.2.0")
	t.Setenv(bundledTAREManifestIDEnv, "tare-cli-0.2.0-a8d74e91")

	cfg := loadCompressionConfig(t, "context-compression:\n  engine: rtk+tare\n")
	if cfg.ContextCompression.Engine != ContextCompressionRTKTARE ||
		cfg.ContextCompression.TARE.BinaryPath != binaryPath || cfg.ContextCompression.TARE.SHA256 != checksum ||
		len(cfg.ContextCompression.TARE.AllowedVersions) != 1 || cfg.ContextCompression.TARE.AllowedVersions[0] != "0.2.0" ||
		cfg.ContextCompression.TARE.ManifestID != "tare-cli-0.2.0-a8d74e91" {
		t.Fatalf("bundled identity not applied for combined engine: %#v", cfg.ContextCompression)
	}
	if err := cfg.ContextCompression.Validate(); err != nil {
		t.Fatalf("combined with bundled identity rejected: %v", err)
	}
}
