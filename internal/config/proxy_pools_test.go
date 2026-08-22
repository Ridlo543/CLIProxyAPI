package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigNormalizesProxyPoolsAndReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `proxy-pools:
  - name: Office
    strategy: ROUND-ROBIN
    strict: true
    cooldown: 30s
    entries:
      - url: socks5h://user:secret@proxy.example:1080
      - url: direct
gemini-api-key:
  - api-key: test
    proxy-pool: office
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProxyPools[0].Strategy; got != "round-robin" {
		t.Fatalf("strategy = %q", got)
	}
	if got := cfg.GeminiKey[0].ProxyPool; got != "office" {
		t.Fatalf("reference = %q", got)
	}
}

func TestLoadConfigRejectsInvalidProxyPoolsWithoutLeakingCredentials(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{"duplicate names", `proxy-pools: [{name: x, strategy: random, entries: [{url: direct}]}, {name: X, strategy: random, entries: [{url: direct}]}]`, "duplicate name"},
		{"invalid strategy", `proxy-pools: [{name: x, strategy: first, entries: [{url: direct}]}]`, "invalid strategy"},
		{"empty entries", `proxy-pools: [{name: x, strategy: random, entries: []}]`, "entries must not be empty"},
		{"invalid duration", `proxy-pools: [{name: x, strategy: random, cooldown: nope, entries: [{url: direct}]}]`, "positive duration"},
		{"duplicate secret URL", `proxy-pools: [{name: x, strategy: random, entries: [{url: 'http://user:secret@host:1'}, {url: 'http://user:secret@host:1'}]}]`, "duplicate entry"},
		{"unknown reference", `gemini-api-key: [{api-key: test, proxy-pool: missing}]`, "unknown proxy pool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("credential leaked: %v", err)
			}
		})
	}
}
