package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestExecuteStream_FirstTokenTimeout_FailsOverToNextCredential(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 0, 3)
	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			FirstTokenTimeout: "50ms",
		},
	}
	m.SetConfig(cfg)

	ids := registerOverloadAuths(t, m, 2)

	var mu sync.Mutex
	var order []string

	m.RegisterExecutor(&customStreamMockExecutor{
		identifier: "codex",
		streamFn: func(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			mu.Lock()
			order = append(order, auth.ID)
			mu.Unlock()

			if auth.ID == ids[0] {
				// Hang indefinitely or until context canceled
				ch := make(chan cliproxyexecutor.StreamChunk)
				go func() {
					<-ctx.Done()
					close(ch)
				}()
				return &cliproxyexecutor.StreamResult{
					Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
					Chunks:  ch,
				}, nil
			}

			// Second credential answers promptly
			ch := make(chan cliproxyexecutor.StreamChunk, 2)
			ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: prompt response\n\n")}
			close(ch)
			return &cliproxyexecutor.StreamResult{
				Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
				Chunks:  ch,
			}, nil
		},
	})

	result, err := m.ExecuteStream(context.Background(), []string{"codex"},
		cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("expected stream to fail over to second credential, got error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result from second credential")
	}

	var payloads [][]byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		if len(chunk.Payload) > 0 {
			payloads = append(payloads, chunk.Payload)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("expected at least 2 attempts (first timed out, second succeeded), got %d: %v", len(order), order)
	}
	if order[0] != ids[0] || order[1] != ids[1] {
		t.Fatalf("attempt order = %v, want [%s, %s]", order, ids[0], ids[1])
	}
	if len(payloads) == 0 {
		t.Fatal("expected payload chunks from second credential")
	}
}
func TestExecuteStream_ExecuteStreamHangs_FailsOverToNextCredential(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 0, 3)
	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			FirstTokenTimeout: "50ms",
		},
	}
	m.SetConfig(cfg)

	ids := registerOverloadAuths(t, m, 2)

	var mu sync.Mutex
	var order []string

	m.RegisterExecutor(&customStreamMockExecutor{
		identifier: "codex",
		streamFn: func(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			mu.Lock()
			order = append(order, auth.ID)
			mu.Unlock()

			if auth.ID == ids[0] {
				// Simulate upstream HTTP dial/headers hang: block until context canceled
				<-ctx.Done()
				return nil, ctx.Err()
			}

			// Second credential answers promptly
			ch := make(chan cliproxyexecutor.StreamChunk, 2)
			ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: prompt response\n\n")}
			close(ch)
			return &cliproxyexecutor.StreamResult{
				Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
				Chunks:  ch,
			}, nil
		},
	})

	result, err := m.ExecuteStream(context.Background(), []string{"codex"},
		cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("expected stream to fail over to second credential, got error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result from second credential")
	}

	var payloads [][]byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		if len(chunk.Payload) > 0 {
			payloads = append(payloads, chunk.Payload)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("expected at least 2 attempts (first timed out in ExecuteStream, second succeeded), got %d: %v", len(order), order)
	}
	if order[0] != ids[0] || order[1] != ids[1] {
		t.Fatalf("attempt order = %v, want [%s, %s]", order, ids[0], ids[1])
	}
	if len(payloads) == 0 {
		t.Fatal("expected payload chunks from second credential")
	}
}
