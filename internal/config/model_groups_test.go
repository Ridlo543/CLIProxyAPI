package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigModelGroupsNormalizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("model-groups:\n  - name: ' code-auto '\n    models:\n      - provider: ' Antigravity '\n        model: ' gemini-test '\n      - provider: codex\n        model: gpt-test\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got := cfg.ModelGroups[0]; got.Name != "code-auto" || got.Models[0].Provider != "antigravity" || got.Models[0].Model != "gemini-test" {
		t.Fatalf("normalized group = %#v", got)
	}
}

func TestNormalizeModelGroupsRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name   string
		groups []ModelGroup
		want   string
	}{
		{"empty name", []ModelGroup{{Models: []ModelGroupMember{{Provider: "p", Model: "m"}}}}, "name must not be empty"},
		{"duplicate group", []ModelGroup{{Name: "auto", Models: []ModelGroupMember{{Provider: "p", Model: "m"}}}, {Name: "AUTO", Models: []ModelGroupMember{{Provider: "q", Model: "n"}}}}, "duplicate group"},
		{"empty members", []ModelGroup{{Name: "auto"}}, "models must not be empty"},
		{"empty target", []ModelGroup{{Name: "auto", Models: []ModelGroupMember{{Provider: "p"}}}}, "must not be empty"},
		{"duplicate member", []ModelGroup{{Name: "auto", Models: []ModelGroupMember{{Provider: "P", Model: "m"}, {Provider: "p", Model: "M"}}}}, "duplicate member"},
		{"group reference", []ModelGroup{{Name: "auto", Models: []ModelGroupMember{{Provider: "p", Model: "other"}}}, {Name: "other", Models: []ModelGroupMember{{Provider: "q", Model: "m"}}}}, "references a model group"},
		{"unknown strategy", []ModelGroup{{Name: "auto", Strategy: "random", Models: []ModelGroupMember{{Provider: "p", Model: "m"}}}}, "strategy must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeModelGroups(tt.groups)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeModelGroupsStrategy(t *testing.T) {
	groups := []ModelGroup{
		{Name: "a", Models: []ModelGroupMember{{Provider: "p", Model: "m"}}},
		{Name: "b", Strategy: " Round-Robin ", Models: []ModelGroupMember{{Provider: "p", Model: "m"}}},
		{Name: "c", Strategy: "ORDERED-FALLBACK", Models: []ModelGroupMember{{Provider: "p", Model: "m"}}},
	}
	got, err := NormalizeModelGroups(groups)
	if err != nil {
		t.Fatalf("NormalizeModelGroups() error = %v", err)
	}
	if got[0].Strategy != "" {
		t.Fatalf("empty strategy = %q, want empty", got[0].Strategy)
	}
	if got[1].Strategy != "round-robin" {
		t.Fatalf("round-robin strategy = %q", got[1].Strategy)
	}
	if got[2].Strategy != "ordered-fallback" {
		t.Fatalf("ordered-fallback strategy = %q", got[2].Strategy)
	}
}
