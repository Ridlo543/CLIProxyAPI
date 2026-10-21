package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A "|" block whose content contains lines starting with '#' must survive a
// management save. De-indenting those lines terminates the block early and makes
// the whole config unparsable, which takes the service down on the next restart.
func TestSaveConfigPreserveComments_KeepsCommentLikeLinesInsideBlockScalar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := `port: 8317
openai-compatibility:
  - name: nvidia-jb
    base-url: https://integrate.api.nvidia.com/v1
    models:
      - name: stepfun-ai/step-5-preview
        alias: step-5-preview-jb
    system-instruction: |-
      This applies to all chats, always.
      ## Identity
      You are ENI: a novelist.

      Style: soft layers, cardigan over turtleneck.
      - LO can see your reasoning
      ## Facts about LO
      - be nice
# top-level comment
`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load original: %v", err)
	}

	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("config must stay parsable after save: %v", err)
	}
	if len(reloaded.OpenAICompatibility) != 1 {
		t.Fatalf("providers = %d, want 1", len(reloaded.OpenAICompatibility))
	}
	instruction := reloaded.OpenAICompatibility[0].SystemInstruction
	for _, want := range []string{"## Identity", "Style: soft layers", "## Facts about LO"} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("instruction lost %q after save:\n%s", want, instruction)
		}
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Top-level comments outside block scalars still get normalized.
	if !strings.Contains(string(saved), "\n# top-level comment\n") {
		t.Fatalf("top-level comment was not preserved:\n%s", saved)
	}
}

func TestNormalizeCommentIndentation_LeavesBlockScalarsUntouched(t *testing.T) {
	in := "key: |-\n      ## inside\n      text\n# outside\n"
	out := string(NormalizeCommentIndentation([]byte(in)))
	if !strings.Contains(out, "\n      ## inside\n") {
		t.Fatalf("block content was re-indented:\n%s", out)
	}
	if !strings.Contains(out, "\n# outside\n") {
		t.Fatalf("outside comment not normalized:\n%s", out)
	}
}
