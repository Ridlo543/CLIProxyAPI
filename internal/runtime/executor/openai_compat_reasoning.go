package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAI-compatible upstreams that run DeepSeek-family models in thinking mode
// reject a follow-up request when an assistant turn that carried tool calls no
// longer carries `reasoning_content`:
//
//	[invalid_request_error] The `reasoning_content` in the thinking mode must be
//	passed back to the API.
//
// Plain Chat Completions clients (OMP, most SDKs) never echo that field back, so
// the router replays the text it observed upstream, keyed by tool call id. Only
// observed text is ever replayed; nothing is invented, and providers that never
// emit reasoning are unaffected because their cache stays empty.

// injectReplayedReasoning fills `reasoning_content` on assistant turns that carry
// tool calls, using reasoning previously observed for the same tool call ids.
// Payloads without tool calls, or with reasoning already present, are returned
// untouched.
func injectReplayedReasoning(payload []byte, provider string) []byte {
	if len(payload) == 0 || !strings.Contains(string(payload), `"tool_calls"`) {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	out := payload
	for index, message := range messages.Array() {
		if message.Get("role").String() != "assistant" {
			continue
		}
		toolCalls := message.Get("tool_calls")
		if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
			continue
		}
		if strings.TrimSpace(message.Get("reasoning_content").String()) != "" {
			continue
		}
		reasoning := ""
		for _, toolCall := range toolCalls.Array() {
			reasoning = cache.ReasoningForToolCall(toolCall.Get("id").String())
			if reasoning != "" {
				break
			}
		}
		if reasoning == "" {
			// No cached text: the upstream still requires the field to be present.
			// Only providers already observed emitting reasoning get the empty
			// marker, so unrelated OpenAI-compatible endpoints keep their payload.
			if !cache.ReasoningCapable(provider) {
				continue
			}
		}
		updated, err := sjson.SetBytes(out, "messages."+strconv.Itoa(index)+".reasoning_content", reasoning)
		if err != nil {
			return payload
		}
		out = updated
	}
	return out
}

// rememberReasoningFromChatCompletion stores reasoning from a non-streaming
// Chat Completions body (or a body reassembled from SSE) against the tool call
// ids it produced.
func rememberReasoningFromChatCompletion(body []byte, provider string) {
	if len(body) == 0 {
		return
	}
	for _, choice := range gjson.GetBytes(body, "choices").Array() {
		message := choice.Get("message")
		if !message.Exists() {
			continue
		}
		reasoning := message.Get("reasoning_content").String()
		if strings.TrimSpace(reasoning) == "" {
			reasoning = message.Get("reasoning").String()
		}
		if strings.TrimSpace(reasoning) == "" {
			continue
		}
		var ids []string
		for _, toolCall := range message.Get("tool_calls").Array() {
			if id := toolCall.Get("id").String(); id != "" {
				ids = append(ids, id)
			}
		}
		cache.MarkReasoningCapable(provider)
		cache.RememberReasoningForToolCalls(ids, reasoning)
	}
}

// reasoningReplayTracker accumulates streamed reasoning and tool call ids from
// upstream Chat Completions frames so a later turn can replay them.
type reasoningReplayTracker struct {
	reasoning strings.Builder
	toolCalls []string
	provider  string
}

// observeFrame records reasoning deltas and tool call ids from one SSE data frame.
func (t *reasoningReplayTracker) observeFrame(data []byte) {
	if t == nil || len(data) == 0 || !strings.Contains(string(data), "delta") {
		return
	}
	for _, choice := range gjson.GetBytes(data, "choices").Array() {
		delta := choice.Get("delta")
		if !delta.Exists() {
			continue
		}
		reasoning := delta.Get("reasoning_content").String()
		if reasoning == "" {
			reasoning = delta.Get("reasoning").String()
		}
		if reasoning != "" {
			t.reasoning.WriteString(reasoning)
		}
		for _, toolCall := range delta.Get("tool_calls").Array() {
			id := toolCall.Get("id").String()
			if id == "" {
				continue
			}
			duplicate := false
			for _, known := range t.toolCalls {
				if known == id {
					duplicate = true
					break
				}
			}
			if !duplicate {
				t.toolCalls = append(t.toolCalls, id)
			}
		}
	}
}

// commit stores the accumulated reasoning for the observed tool call ids.
func (t *reasoningReplayTracker) commit() {
	if t == nil {
		return
	}
	if strings.TrimSpace(t.reasoning.String()) != "" {
		cache.MarkReasoningCapable(t.provider)
	}
	if len(t.toolCalls) == 0 {
		return
	}
	cache.RememberReasoningForToolCalls(t.toolCalls, t.reasoning.String())
}

// reasoningReplayRequired reports whether an upstream error is the DeepSeek-family
// rejection for a follow-up turn that dropped reasoning_content.
func reasoningReplayRequired(status int, body []byte) bool {
	if status != http.StatusBadRequest || len(body) == 0 {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "reasoning_content") && strings.Contains(text, "must be passed back")
}

// injectReplayedReasoningForce behaves like injectReplayedReasoning but always
// writes the field, including the empty marker when no reasoning was cached.
// It is only used after the upstream explicitly demanded the field.
func injectReplayedReasoningForce(payload []byte, provider string) []byte {
	if !isJSONObject(payload) {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	out := payload
	for index, message := range messages.Array() {
		if message.Get("role").String() != "assistant" {
			continue
		}
		toolCalls := message.Get("tool_calls")
		if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
			continue
		}
		if strings.TrimSpace(message.Get("reasoning_content").String()) != "" {
			continue
		}
		reasoning := ""
		for _, toolCall := range toolCalls.Array() {
			if reasoning = cache.ReasoningForToolCall(toolCall.Get("id").String()); reasoning != "" {
				break
			}
		}
		updated, err := sjson.SetBytes(out, "messages."+strconv.Itoa(index)+".reasoning_content", reasoning)
		if err != nil {
			return payload
		}
		out = updated
	}
	cache.MarkReasoningCapable(provider)
	return out
}

// replayRequestWithPayload clones an outgoing request with a replaced body so it
// can be replayed after the reasoning markers were injected.
func replayRequestWithPayload(ctx context.Context, req *http.Request, payload []byte) *http.Request {
	clone := req.Clone(ctx)
	clone.Body = io.NopCloser(bytes.NewReader(payload))
	clone.ContentLength = int64(len(payload))
	clone.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	return clone
}

func isJSONObject(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	return len(trimmed) > 0 && trimmed[0] == '{'
}
