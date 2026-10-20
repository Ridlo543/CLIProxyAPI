package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestEffectiveFirstTokenTimeout_ScalesWithPromptSize(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{FirstTokenTimeout: "45s"},
	})

	if got := m.effectiveFirstTokenTimeout("codex", "gpt-5.6-terra", nil); got != 45*time.Second {
		t.Fatalf("empty payload timeout = %v, want 45s (base must never shrink)", got)
	}

	small := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	if got := m.effectiveFirstTokenTimeout("codex", "gpt-5.6-terra", small); got != 45*time.Second {
		t.Fatalf("small payload timeout = %v, want 45s", got)
	}

	// 300k characters at the conservative 3 bytes/token ratio = 100k tokens = +10s.
	large := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("a", 300000) + `"}]}`)
	if got := m.effectiveFirstTokenTimeout("codex", "gpt-5.6-terra", large); got != 55*time.Second {
		t.Fatalf("large payload timeout = %v, want 55s", got)
	}

	// The widening is capped so a huge prompt cannot hide a dead credential forever.
	huge := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("a", 30000000) + `"}]}`)
	if got := m.effectiveFirstTokenTimeout("codex", "gpt-5.6-terra", huge); got != 165*time.Second {
		t.Fatalf("huge payload timeout = %v, want 165s (45s base + 120s cap)", got)
	}
}

func TestEffectiveFirstTokenTimeout_WidensForReasoningModels(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{FirstTokenTimeout: "45s"},
	})

	cases := []struct {
		model   string
		payload string
		want    time.Duration
	}{
		{"gemini-3.8-flash", "", 45 * time.Second},
		{"gemini-3.8-flash-high", "", 67500 * time.Millisecond},
		{"claude-opus-4-6-thinking", "", 67500 * time.Millisecond},
		{"deepseek-v4-pro", "", 67500 * time.Millisecond},
		{"gemini-3.8-flash-medium", "", 56250 * time.Millisecond},
		{"gemini-3.8-flash", `{"reasoning_effort":"high"}`, 67500 * time.Millisecond},
		{"gemini-3.8-flash", `{"reasoning_effort":"medium"}`, 56250 * time.Millisecond},
		{"gemini-3.8-flash", `{"reasoning_effort":"low"}`, 45 * time.Second},
	}
	for _, tc := range cases {
		got := m.effectiveFirstTokenTimeout("antigravity", tc.model, []byte(tc.payload))
		if got != tc.want {
			t.Errorf("model %q effort %q timeout = %v, want %v", tc.model, tc.payload, got, tc.want)
		}
	}

	// The multiplier must never shrink the configured base, whatever the model name.
	for _, model := range []string{"gpt-5.6-terra", "gemini-3.8-flash-low", "unknown-model"} {
		if got := m.effectiveFirstTokenTimeout("antigravity", model, nil); got < 45*time.Second {
			t.Errorf("model %q timeout = %v, must never fall below the configured base", model, got)
		}
	}
}

func TestEstimatePromptTokens_IgnoresImages(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"` + strings.Repeat("a", 3000) + `"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", 100000) + `"}}` +
		`]}]}`)
	if got := estimatePromptTokens(payload); got != 1000 {
		t.Fatalf("estimate = %d tokens, want 1000 (base64 image bytes must not inflate the deadline)", got)
	}
}

// A large prompt that legitimately needs more than the base ceiling must not be
// failed over: the deadline scales with prompt size instead of cutting it off.
func TestExecuteStream_LargePromptNotCancelledByBaseDeadline(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 0, 3)
	m.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{FirstTokenTimeout: "50ms"},
	})
	registerOverloadAuths(t, m, 2)

	var mu sync.Mutex
	var attempts int
	m.RegisterExecutor(&customStreamMockExecutor{
		identifier: "codex",
		streamFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			mu.Lock()
			attempts++
			mu.Unlock()
			// Base deadline is 50ms; the scaled deadline for this payload is >1s.
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			ch := make(chan cliproxyexecutor.StreamChunk, 2)
			ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: large prompt answer\n\n")}
			close(ch)
			return &cliproxyexecutor.StreamResult{
				Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
				Chunks:  ch,
			}, nil
		},
	})

	payload := []byte(`{"model":"gpt-5.6-terra","messages":[{"role":"user","content":"` + strings.Repeat("a", 9000) + `"}]}`)
	result, err := m.ExecuteStream(context.Background(), []string{"codex"},
		cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: payload}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("large prompt must not be failed over at the base deadline, got error: %v", err)
	}
	if result == nil {
		t.Fatal("expected stream result")
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no failover for a slow large-prompt first token)", attempts)
	}
}

// A stream that stops producing chunks after the first payload must be aborted
// instead of hanging forever; the abort is reported as a stream error.
func TestExecuteStream_StreamIdleAbortsSilentStream(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 0, 3)
	m.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{StreamIdleTimeout: "100ms"},
	})
	registerOverloadAuths(t, m, 1)

	upstreamCanceled := make(chan struct{}, 1)
	m.RegisterExecutor(&customStreamMockExecutor{
		identifier: "codex",
		streamFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			ch := make(chan cliproxyexecutor.StreamChunk, 1)
			ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: first\n\n")}
			go func() {
				<-ctx.Done()
				upstreamCanceled <- struct{}{}
				close(ch)
			}()
			return &cliproxyexecutor.StreamResult{
				Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
				Chunks:  ch,
			}, nil
		},
	})

	result, err := m.ExecuteStream(context.Background(), []string{"codex"},
		cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)},
		cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v, want stream result", err)
	}
	if result == nil {
		t.Fatal("expected stream result")
	}

	started := time.Now()
	var gotIdle bool
	for chunk := range result.Chunks {
		if chunk.Err == nil {
			continue
		}
		var streamErr *Error
		if !asAuthError(chunk.Err, &streamErr) {
			t.Fatalf("idle abort error = %T, want *Error", chunk.Err)
		}
		if streamErr.Code != ErrorCodeStreamIdleTimeout {
			t.Fatalf("error code = %q, want %q", streamErr.Code, ErrorCodeStreamIdleTimeout)
		}
		gotIdle = true
	}
	drained := time.Since(started)
	if !gotIdle {
		t.Fatal("expected stream_idle_timeout error chunk")
	}
	if drained > 5*time.Second {
		t.Fatalf("idle abort took %v, want well under 5s", drained)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("idle abort must cancel the upstream context")
	}
}

// asAuthError unwraps a stream chunk error into the manager's *Error type.
func asAuthError(err error, target **Error) bool {
	for err != nil {
		if typed, ok := err.(*Error); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		next := unwrapper.Unwrap()
		if next == err {
			return false
		}
		err = next
	}
	return false
}
