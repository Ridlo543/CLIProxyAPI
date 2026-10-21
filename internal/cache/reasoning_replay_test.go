package cache

import (
	"testing"
	"time"
)

func TestReasoningReplay_RoundTripPerToolCall(t *testing.T) {
	ResetReasoningReplayCache()
	t.Cleanup(ResetReasoningReplayCache)

	RememberReasoningForToolCalls([]string{"call_a", "call_b"}, "  thinking hard  ")
	for _, id := range []string{"call_a", "call_b"} {
		if got := ReasoningForToolCall(id); got != "thinking hard" {
			t.Fatalf("ReasoningForToolCall(%q) = %q, want trimmed reasoning", id, got)
		}
	}
	if got := ReasoningForToolCall("call_unknown"); got != "" {
		t.Fatalf("unknown id returned %q, want empty", got)
	}
}

func TestReasoningReplay_IgnoresEmptyInput(t *testing.T) {
	ResetReasoningReplayCache()
	t.Cleanup(ResetReasoningReplayCache)

	RememberReasoningForToolCalls([]string{"call_a"}, "   ")
	RememberReasoningForToolCalls(nil, "reasoning")
	RememberReasoningForToolCalls([]string{"  "}, "reasoning")

	if got := ReasoningForToolCall("call_a"); got != "" {
		t.Fatalf("empty reasoning must not be cached, got %q", got)
	}
}

func TestReasoningReplay_Expires(t *testing.T) {
	ResetReasoningReplayCache()
	t.Cleanup(func() {
		reasoningReplayNow = time.Now
		ResetReasoningReplayCache()
	})

	base := time.Now()
	reasoningReplayNow = func() time.Time { return base }
	RememberReasoningForToolCalls([]string{"call_a"}, "reasoning")

	reasoningReplayNow = func() time.Time { return base.Add(ReasoningReplayTTL - time.Second) }
	if got := ReasoningForToolCall("call_a"); got != "reasoning" {
		t.Fatalf("before expiry got %q, want reasoning", got)
	}

	reasoningReplayNow = func() time.Time { return base.Add(ReasoningReplayTTL + time.Second) }
	if got := ReasoningForToolCall("call_a"); got != "" {
		t.Fatalf("after expiry got %q, want empty", got)
	}
}

func TestReasoningReplay_EvictsOldestBeyondLimit(t *testing.T) {
	ResetReasoningReplayCache()
	t.Cleanup(ResetReasoningReplayCache)

	for i := range ReasoningReplayMaxEntries + 10 {
		RememberReasoningForToolCalls([]string{"call_" + time.Duration(i).String()}, "reasoning")
	}
	if len(reasoningReplayEntries) > ReasoningReplayMaxEntries {
		t.Fatalf("cache grew to %d entries, limit %d", len(reasoningReplayEntries), ReasoningReplayMaxEntries)
	}
}
