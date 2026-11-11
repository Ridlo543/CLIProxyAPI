package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This is the exact shape a JavaScript YAML emitter produces for a string with
// CRLF line breaks: escaped \r plus real line breaks inside a double-quoted
// scalar. The continuation line starting with '#' is data, not a comment.
const multilineQuotedInstruction = "codex:\n" +
	"  # real comment\n" +
	"  system-instruction: \"FROM codex:gpt\\r\n" +
	"\n" +
	"    \\r\n" +
	"\n" +
	"    # Deployment Configuration\\r\n" +
	"\n" +
	"    preserved text\"\n" +
	"  single: 'first line\n" +
	"\n" +
	"    # still data, it''s quoted\n" +
	"\n" +
	"    last'\n"

func TestNormalizeCommentIndentation_KeepsQuotedContinuationLines(t *testing.T) {
	out := string(NormalizeCommentIndentation([]byte(multilineQuotedInstruction)))
	for _, kept := range []string{"\n    # Deployment Configuration\\r\n", "\n    # still data, it''s quoted\n"} {
		if !strings.Contains(out, kept) {
			t.Fatalf("quoted continuation line was re-indented, want %q in:\n%s", kept, out)
		}
	}
	if !strings.Contains(out, "\n# real comment\n") {
		t.Fatalf("real comment was not left aligned:\n%s", out)
	}
	var parsed struct {
		Codex struct {
			Instruction string `yaml:"system-instruction"`
			Single      string `yaml:"single"`
		} `yaml:"codex"`
	}
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("normalized output does not parse: %v", err)
	}
	if want := "FROM codex:gpt\r\n\r\n# Deployment Configuration\r\npreserved text"; parsed.Codex.Instruction != want {
		t.Fatalf("instruction = %q, want %q", parsed.Codex.Instruction, want)
	}
	if want := "first line\n# still data, it's quoted\nlast"; parsed.Codex.Single != want {
		t.Fatalf("single = %q, want %q", parsed.Codex.Single, want)
	}
}

func TestNormalizeCommentIndentation_LeavesUnparseableInputUntouched(t *testing.T) {
	in := "a: [\n  # comment\n"
	if out := string(NormalizeCommentIndentation([]byte(in))); out != in {
		t.Fatalf("unparseable input changed:\n%s", out)
	}
}

func TestNormalizeCommentIndentation_UnicodeKeyBeforeQuotedScalar(t *testing.T) {
	in := "ключ: \"a\n\n  # data\n\n  b\"\nnext:\n  # comment\n  x: 1\n"
	out := string(NormalizeCommentIndentation([]byte(in)))
	if !strings.Contains(out, "\n  # data\n") {
		t.Fatalf("quoted data after a multibyte key was re-indented:\n%s", out)
	}
	if !strings.Contains(out, "\n# comment\n") {
		t.Fatalf("comment was not left aligned:\n%s", out)
	}
}
