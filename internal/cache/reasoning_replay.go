package cache

import (
	"strings"
	"sync"
	"time"
)

const (
	// ReasoningReplayTTL bounds how long a streamed reasoning summary stays
	// available for replay. DeepSeek-family providers require the reasoning of a
	// tool-calling assistant turn to be sent back on the next turn, and clients
	// that speak plain Chat Completions never echo it.
	ReasoningReplayTTL = 2 * time.Hour

	// ReasoningReplayMaxEntries bounds process memory. Oldest entries go first.
	ReasoningReplayMaxEntries = 8192
)

type reasoningReplayEntry struct {
	Reasoning string
	Expires   time.Time
	Sequence  uint64
}

var (
	reasoningReplayMu      sync.Mutex
	reasoningReplayEntries = make(map[string]reasoningReplayEntry)
	reasoningReplaySeq     uint64
	reasoningReplayNow     = time.Now
)

// RememberReasoningForToolCalls stores the reasoning text that produced the given
// tool calls. Empty reasoning or empty ids are ignored: only text the router
// actually observed upstream is ever replayed.
func RememberReasoningForToolCalls(toolCallIDs []string, reasoning string) {
	reasoning = strings.TrimSpace(reasoning)
	if reasoning == "" {
		return
	}
	ids := make([]string, 0, len(toolCallIDs))
	for _, id := range toolCallIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}

	now := reasoningReplayNow()
	reasoningReplayMu.Lock()
	defer reasoningReplayMu.Unlock()
	for _, id := range ids {
		reasoningReplaySeq++
		reasoningReplayEntries[id] = reasoningReplayEntry{
			Reasoning: reasoning,
			Expires:   now.Add(ReasoningReplayTTL),
			Sequence:  reasoningReplaySeq,
		}
	}
	evictReasoningReplayLocked(now)
}

// ReasoningForToolCall returns the stored reasoning for a tool call id, or "".
func ReasoningForToolCall(toolCallID string) string {
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		return ""
	}
	now := reasoningReplayNow()
	reasoningReplayMu.Lock()
	defer reasoningReplayMu.Unlock()
	entry, ok := reasoningReplayEntries[toolCallID]
	if !ok {
		return ""
	}
	if !entry.Expires.After(now) {
		delete(reasoningReplayEntries, toolCallID)
		return ""
	}
	return entry.Reasoning
}

func evictReasoningReplayLocked(now time.Time) {
	if len(reasoningReplayEntries) > ReasoningReplayMaxEntries {
		var oldestID string
		var oldestSeq uint64
		for id, entry := range reasoningReplayEntries {
			if !entry.Expires.After(now) {
				delete(reasoningReplayEntries, id)
				continue
			}
			if oldestID == "" || entry.Sequence < oldestSeq {
				oldestID, oldestSeq = id, entry.Sequence
			}
		}
		for len(reasoningReplayEntries) > ReasoningReplayMaxEntries && oldestID != "" {
			delete(reasoningReplayEntries, oldestID)
			oldestID = ""
			for id, entry := range reasoningReplayEntries {
				if oldestID == "" || entry.Sequence < oldestSeq {
					oldestID, oldestSeq = id, entry.Sequence
				}
			}
		}
		return
	}
	// Opportunistically drop expired entries instead of letting them linger.
	for id, entry := range reasoningReplayEntries {
		if !entry.Expires.After(now) {
			delete(reasoningReplayEntries, id)
		}
	}
}

// ResetReasoningReplayCache clears the cache. Used by tests.
func ResetReasoningReplayCache() {
	reasoningReplayMu.Lock()
	defer reasoningReplayMu.Unlock()
	reasoningReplayEntries = make(map[string]reasoningReplayEntry)
	reasoningReplaySeq = 0
}

const (
	// reasoningCapableTTL bounds how long the "this upstream emits reasoning"
	// mark survives. It is refreshed by every reasoning-bearing response, so a
	// live provider stays marked while idle ones age out.
	reasoningCapableTTL = 24 * time.Hour
)

var (
	reasoningCapableMu      sync.Mutex
	reasoningCapableEntries = make(map[string]time.Time)
)

// MarkReasoningCapable records that the given upstream/provider has produced
// reasoning content. It authorizes the empty-marker fallback for that provider,
// which keeps follow-up turns valid even when the original reasoning was not
// cached (for example after a restart or from a different router instance).
func MarkReasoningCapable(provider string) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return
	}
	reasoningCapableMu.Lock()
	defer reasoningCapableMu.Unlock()
	reasoningCapableEntries[provider] = reasoningReplayNow().Add(reasoningCapableTTL)
}

// ReasoningCapable reports whether the provider has recently produced reasoning.
func ReasoningCapable(provider string) bool {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return false
	}
	now := reasoningReplayNow()
	reasoningCapableMu.Lock()
	defer reasoningCapableMu.Unlock()
	expires, ok := reasoningCapableEntries[provider]
	if !ok {
		return false
	}
	if !expires.After(now) {
		delete(reasoningCapableEntries, provider)
		return false
	}
	return true
}
