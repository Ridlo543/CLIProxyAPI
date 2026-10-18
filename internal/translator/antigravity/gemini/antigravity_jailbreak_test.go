package gemini

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func TestInjectSystemInstruction_NoConfig(t *testing.T) {
	input := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	got := InjectSystemInstruction("gemini-3.8-flash", input, nil)
	if gjson.GetBytes(got, "request.systemInstruction").Exists() {
		t.Fatalf("nil config must not inject, got %s", got)
	}
}

func TestInjectSystemInstruction_EmptyInstruction(t *testing.T) {
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "",
		SystemInstructionModels: []string{"gemini-3.8-flash"},
	}
	input := []byte(`{"request":{"contents":[]}}`)
	got := InjectSystemInstruction("gemini-3.8-flash", input, cfg)
	if gjson.GetBytes(got, "request.systemInstruction").Exists() {
		t.Fatalf("empty instruction must not inject, got %s", got)
	}
}

func TestInjectSystemInstruction_EmptyModelList(t *testing.T) {
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "be ENI",
		SystemInstructionModels: nil,
	}
	input := []byte(`{"request":{"contents":[]}}`)
	got := InjectSystemInstruction("gemini-3.8-flash", input, cfg)
	if gjson.GetBytes(got, "request.systemInstruction").Exists() {
		t.Fatalf("empty model list must not inject, got %s", got)
	}
}

func TestInjectSystemInstruction_ModelNotMatched(t *testing.T) {
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "be ENI",
		SystemInstructionModels: []string{"gemini-3.8-flash"},
	}
	input := []byte(`{"request":{"contents":[]}}`)
	got := InjectSystemInstruction("gemini-3.7-flash", input, cfg)
	if gjson.GetBytes(got, "request.systemInstruction").Exists() {
		t.Fatalf("non-matching model must not inject, got %s", got)
	}
}

func TestInjectSystemInstruction_InjectsWhenNoExistingInstruction(t *testing.T) {
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "You are ENI.",
		SystemInstructionModels: []string{"gemini-3.8-flash"},
	}
	input := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	got := InjectSystemInstruction("gemini-3.8-flash", input, cfg)

	text := gjson.GetBytes(got, "request.systemInstruction.parts.0.text").String()
	if text != "You are ENI." {
		t.Fatalf("injected text = %q, want %q", text, "You are ENI.")
	}
	if !gjson.GetBytes(got, "request.contents.0.parts.0.text").Exists() {
		t.Fatalf("original contents must be preserved, got %s", got)
	}
}

func TestInjectSystemInstruction_AppendsToExistingInstruction(t *testing.T) {
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "You are ENI.",
		SystemInstructionModels: []string{"gemini-3.8-flash"},
	}
	input := []byte(`{"request":{"systemInstruction":{"role":"user","parts":[{"text":"be terse"}]}}}`)
	got := InjectSystemInstruction("gemini-3.8-flash", input, cfg)

	parts := gjson.GetBytes(got, "request.systemInstruction.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d: %s", len(parts), got)
	}
	if first := parts[0].Get("text").String(); first != "You are ENI." {
		t.Fatalf("first part = %q, want injected instruction first", first)
	}
	if second := parts[1].Get("text").String(); second != "be terse" {
		t.Fatalf("second part = %q, want client instruction preserved", second)
	}
}

func TestInjectSystemInstruction_DoesNotLeakToSiblingModels(t *testing.T) {
	// Exact matching: a persona configured for gemini-3.8-flash must never
	// reach sibling upstream models that merely share the prefix.
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "You are ENI.",
		SystemInstructionModels: []string{"gemini-3.8-flash"},
	}
	for _, sibling := range []string{
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-tiered",
		"gemini-3.8-flash-thinking",
		"gemini-3.7-flash",
	} {
		got := InjectSystemInstruction(sibling, []byte(`{"request":{"contents":[]}}`), cfg)
		if gjson.GetBytes(got, "request.systemInstruction").Exists() {
			t.Fatalf("sibling %q must not receive the instruction, got %s", sibling, got)
		}
	}
}

func TestInjectSystemInstruction_JailbreakAliasIsSeparateEntity(t *testing.T) {
	// A dedicated jailbreak alias is configured explicitly, so it receives the
	// persona while the plain model stays clean.
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "You are ENI.",
		SystemInstructionModels: []string{"gemini-3.8-flash-jb"},
	}
	plain := InjectSystemInstruction("gemini-3.8-flash", []byte(`{"request":{"contents":[]}}`), cfg)
	if gjson.GetBytes(plain, "request.systemInstruction").Exists() {
		t.Fatalf("plain model must stay clean, got %s", plain)
	}
	jb := InjectSystemInstruction("gemini-3.8-flash-jb", []byte(`{"request":{"contents":[]}}`), cfg)
	if !gjson.GetBytes(jb, "request.systemInstruction.parts.0.text").Exists() {
		t.Fatalf("jailbreak alias must receive the persona, got %s", jb)
	}
}

func TestInjectSystemInstruction_CaseInsensitiveMatch(t *testing.T) {
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "You are ENI.",
		SystemInstructionModels: []string{"GEMINI-3.8-FLASH"},
	}
	input := []byte(`{"request":{"contents":[]}}`)
	got := InjectSystemInstruction("gemini-3.8-flash", input, cfg)
	if !gjson.GetBytes(got, "request.systemInstruction.parts.0.text").Exists() {
		t.Fatalf("matching must be case-insensitive, got %s", got)
	}
}

func TestInjectSystemInstruction_ExistingInstructionWithoutParts(t *testing.T) {
	cfg := &config.AntigravityConfig{
		SystemInstruction:       "You are ENI.",
		SystemInstructionModels: []string{"gemini-3.8-flash"},
	}
	// A systemInstruction that carries no parts array is replaced, not merged.
	input := []byte(`{"request":{"systemInstruction":{"role":"user"}}}`)
	got := InjectSystemInstruction("gemini-3.8-flash", input, cfg)

	parts := gjson.GetBytes(got, "request.systemInstruction.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d: %s", len(parts), got)
	}
	if text := parts[0].Get("text").String(); text != "You are ENI." {
		t.Fatalf("text = %q, want injected instruction", text)
	}
}

func TestInjectSystemInstruction_LongMultilineInstruction(t *testing.T) {
	long := strings.Repeat("line of persona text\n", 500)
	cfg := &config.AntigravityConfig{
		SystemInstruction:       long,
		SystemInstructionModels: []string{"gemini-3.8-flash"},
	}
	input := []byte(`{"request":{"contents":[]}}`)
	got := InjectSystemInstruction("gemini-3.8-flash", input, cfg)

	text := gjson.GetBytes(got, "request.systemInstruction.parts.0.text").String()
	if text != long {
		t.Fatalf("long instruction not preserved verbatim (len %d vs %d)", len(text), len(long))
	}
}
