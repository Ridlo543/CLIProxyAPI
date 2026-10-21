package executor

import (
	"bytes"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/tidwall/gjson"
)

func TestInjectReplayedReasoning_FillsToolCallTurn(t *testing.T) {
	cache.ResetReasoningReplayCache()
	t.Cleanup(cache.ResetReasoningReplayCache)
	cache.RememberReasoningForToolCalls([]string{"call_1"}, "I should read the file first.")

	payload := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[
		{"role":"user","content":"read main.go"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"package main"}
	]}`)

	out := injectReplayedReasoning(payload, "openai-compatible-test")
	if got := gjson.GetBytes(out, "messages.1.reasoning_content").String(); got != "I should read the file first." {
		t.Fatalf("reasoning_content = %q, want the cached reasoning", got)
	}
}

func TestInjectReplayedReasoning_LeavesOtherTurnsAlone(t *testing.T) {
	cache.ResetReasoningReplayCache()
	t.Cleanup(cache.ResetReasoningReplayCache)
	cache.RememberReasoningForToolCalls([]string{"call_1"}, "cached")

	payload := []byte(`{"messages":[
		{"role":"assistant","content":"plain answer"},
		{"role":"assistant","content":"","reasoning_content":"client supplied","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_unknown","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"user","content":"hi"}
	]}`)

	out := injectReplayedReasoning(payload, "openai-compatible-test")
	if got := gjson.GetBytes(out, "messages.0.reasoning_content"); got.Exists() {
		t.Fatalf("text-only assistant must not gain reasoning_content, got %q", got.String())
	}
	if got := gjson.GetBytes(out, "messages.1.reasoning_content").String(); got != "client supplied" {
		t.Fatalf("client reasoning was overwritten: %q", got)
	}
	if got := gjson.GetBytes(out, "messages.2.reasoning_content"); got.Exists() {
		t.Fatalf("unknown tool call must not gain reasoning_content, got %q", got.String())
	}
}

func TestInjectReplayedReasoning_PassesThroughUntouchedPayloads(t *testing.T) {
	cache.ResetReasoningReplayCache()
	t.Cleanup(cache.ResetReasoningReplayCache)

	for _, payload := range [][]byte{
		nil,
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		[]byte(`not json at all`),
		[]byte(`{"input":"hello"}`),
	} {
		if got := injectReplayedReasoning(payload, "openai-compatible-test"); !bytes.Equal(got, payload) {
			t.Fatalf("payload changed unexpectedly: %s -> %s", payload, got)
		}
	}
}

func TestRememberReasoningFromChatCompletion_NonStream(t *testing.T) {
	cache.ResetReasoningReplayCache()
	t.Cleanup(cache.ResetReasoningReplayCache)

	body := []byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"plan the edit","tool_calls":[{"id":"call_9","type":"function","function":{"name":"edit","arguments":"{}"}}]}}]}`)
	rememberReasoningFromChatCompletion(body, "openai-compatible-test")

	if got := cache.ReasoningForToolCall("call_9"); got != "plan the edit" {
		t.Fatalf("cached reasoning = %q, want plan the edit", got)
	}
}

func TestReasoningReplayTracker_AccumulatesStreamedDeltas(t *testing.T) {
	cache.ResetReasoningReplayCache()
	t.Cleanup(cache.ResetReasoningReplayCache)

	tracker := reasoningReplayTracker{provider: "openai-compatible-test"}
	tracker.observeFrame([]byte(`{"choices":[{"delta":{"reasoning_content":"step "}}]}`))
	tracker.observeFrame([]byte(`{"choices":[{"delta":{"reasoning":"two"}}]}`))
	tracker.observeFrame([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x"}]}}]}`))
	tracker.observeFrame([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x"}]}}]}`))
	tracker.observeFrame([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_y"}]}}]}`))
	tracker.commit()

	for _, id := range []string{"call_x", "call_y"} {
		if got := cache.ReasoningForToolCall(id); got != "step two" {
			t.Fatalf("ReasoningForToolCall(%q) = %q, want step two", id, got)
		}
	}
}

func TestReasoningReplayTracker_CommitWithoutToolCallsIsNoop(t *testing.T) {
	cache.ResetReasoningReplayCache()
	t.Cleanup(cache.ResetReasoningReplayCache)

	tracker := reasoningReplayTracker{provider: "openai-compatible-test"}
	tracker.observeFrame([]byte(`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`))
	tracker.commit()

	if len(cache.ReasoningForToolCall("")) != 0 {
		t.Fatal("unexpected cache write")
	}
}

func TestInjectReplayedReasoning_EmptyMarkerOnlyForReasoningProviders(t *testing.T) {
	cache.ResetReasoningReplayCache()
	t.Cleanup(cache.ResetReasoningReplayCache)

	payload := []byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_none","type":"function","function":{"name":"read","arguments":"{}"}}]}]}`)

	if got := injectReplayedReasoning(payload, "openai-compatible-cold"); !bytes.Equal(got, payload) {
		t.Fatalf("provider without reasoning history must be untouched, got %s", got)
	}

	cache.MarkReasoningCapable("openai-compatible-warm")
	out := injectReplayedReasoning(payload, "openai-compatible-warm")
	if got := gjson.GetBytes(out, "messages.0.reasoning_content"); !got.Exists() || got.String() != "" {
		t.Fatalf("reasoning provider must receive the empty marker, got %v", got)
	}
}

func TestReasoningReplayTracker_MarksProvider(t *testing.T) {
	cache.ResetReasoningReplayCache()
	t.Cleanup(cache.ResetReasoningReplayCache)

	tracker := reasoningReplayTracker{provider: "openai-compatible-stream"}
	tracker.observeFrame([]byte(`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`))
	tracker.commit()

	if !cache.ReasoningCapable("openai-compatible-stream") {
		t.Fatal("provider should be marked reasoning-capable after a reasoning delta")
	}
}

func TestReasoningReplayRequired_MatchesOnlyTheThinkingModeRejection(t *testing.T) {
	hit := []byte(`{"error":{"message":"Upstream request failed: [invalid_request_error] The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API."}}`)
	if !reasoningReplayRequired(400, hit) {
		t.Fatal("expected the thinking-mode rejection to be detected")
	}
	if reasoningReplayRequired(500, hit) {
		t.Fatal("only 400 should be retried")
	}
	if reasoningReplayRequired(400, []byte(`{"error":{"message":"unknown tool"}}`)) {
		t.Fatal("unrelated 400 must not be retried")
	}
	if reasoningReplayRequired(400, nil) {
		t.Fatal("empty body must not be retried")
	}
}

func TestInjectReplayedReasoningForce_WritesEmptyMarkerWithoutCache(t *testing.T) {
	cache.ResetReasoningReplayCache()
	t.Cleanup(cache.ResetReasoningReplayCache)

	payload := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_missing","type":"function","function":{"name":"read","arguments":"{}"}}]}
	]}`)

	if got := injectReplayedReasoning(payload, "openai-compatible-x"); !bytes.Equal(got, payload) {
		t.Fatal("gated injection must leave a cold provider untouched")
	}

	out := injectReplayedReasoningForce(payload, "openai-compatible-x")
	if got := gjson.GetBytes(out, "messages.1.reasoning_content"); !got.Exists() || got.String() != "" {
		t.Fatalf("forced injection must write the empty marker, got %v", got)
	}
	if !cache.ReasoningCapable("openai-compatible-x") {
		t.Fatal("forced injection must mark the provider")
	}
}
