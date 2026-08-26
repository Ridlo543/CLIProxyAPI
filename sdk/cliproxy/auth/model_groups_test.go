package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type groupExecutor struct {
	provider  string
	err       error
	streamErr error
	stream    *cliproxyexecutor.StreamResult
	attempts  *[]string
}

func (e *groupExecutor) Identifier() string { return e.provider }
func (e *groupExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	*e.attempts = append(*e.attempts, e.provider+"/"+req.Model)
	return cliproxyexecutor.Response{Payload: []byte(e.provider)}, e.err
}
func (e *groupExecutor) ExecuteStream(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	*e.attempts = append(*e.attempts, e.provider+"/"+req.Model)
	return e.stream, e.streamErr
}
func (*groupExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }
func (*groupExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (*groupExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func groupManager(t *testing.T, executors ...*groupExecutor) *Manager {
	t.Helper()
	m := NewManager(nil, nil, nil)
	for _, executor := range executors {
		m.RegisterExecutor(executor)
		registry.GetGlobalRegistry().RegisterClient(executor.provider, executor.provider, []*registry.ModelInfo{{ID: "one"}, {ID: "two"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(executor.provider) })
		if _, err := m.Register(context.Background(), &Auth{ID: executor.provider, Provider: executor.provider, Status: StatusActive, ModelStates: map[string]*ModelState{
			"one": {Status: StatusActive}, "two": {Status: StatusActive},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func TestExecuteModelGroupOrderedFallbackAndNonRetryableStop(t *testing.T) {
	t.Run("ordered fallback on retryable error", func(t *testing.T) {
		var attempts []string
		first := &groupExecutor{provider: "first", err: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "down"}, attempts: &attempts}
		second := &groupExecutor{provider: "second", attempts: &attempts}
		m := groupManager(t, first, second)
		targets := []ModelGroupTarget{{Provider: "first", Model: "one"}, {Provider: "second", Model: "two"}}
		resp, err := m.ExecuteModelGroup(context.Background(), targets, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
		if err != nil || string(resp.Payload) != "second" || len(attempts) != 2 || attempts[0] != "first/one" || attempts[1] != "second/two" {
			t.Fatalf("response = %q, error = %v, attempts = %v", resp.Payload, err, attempts)
		}
	})
	t.Run("non-retryable error stops immediately", func(t *testing.T) {
		// Fresh manager: the fallback case above legitimately cools the first
		// credential down after its 503, and availability must be honored.
		var attempts []string
		first := &groupExecutor{provider: "first", err: &Error{HTTPStatus: http.StatusBadRequest, Message: "bad"}, attempts: &attempts}
		second := &groupExecutor{provider: "second", attempts: &attempts}
		m := groupManager(t, first, second)
		targets := []ModelGroupTarget{{Provider: "first", Model: "one"}, {Provider: "second", Model: "two"}}
		_, err := m.ExecuteModelGroup(context.Background(), targets, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
		if err == nil || len(attempts) != 1 {
			t.Fatalf("error = %v, attempts = %v", err, attempts)
		}
	})
}

func TestExecuteModelGroupCancellationStopsImmediately(t *testing.T) {
	var attempts []string
	m := groupManager(t, &groupExecutor{provider: "first", attempts: &attempts})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.ExecuteModelGroup(ctx, []ModelGroupTarget{{Provider: "first", Model: "one"}}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if !errors.Is(err, context.Canceled) || len(attempts) != 0 {
		t.Fatalf("error = %v, attempts = %v", err, attempts)
	}
}

func TestExecuteStreamModelGroupFallsBackOnlyBeforeResult(t *testing.T) {
	var attempts []string
	bootstrapStream := func() *cliproxyexecutor.StreamResult {
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data")}
		close(chunks)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}
	}
	assertBootstrap := func(result *cliproxyexecutor.StreamResult) {
		t.Helper()
		select {
		case chunk := <-result.Chunks:
			if string(chunk.Payload) != "data" {
				t.Fatalf("bootstrap payload = %q", chunk.Payload)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out reading stream bootstrap")
		}
	}
	first := &groupExecutor{provider: "first", streamErr: &Error{HTTPStatus: http.StatusBadGateway, Message: "bootstrap"}, attempts: &attempts}
	second := &groupExecutor{provider: "second", stream: bootstrapStream(), attempts: &attempts}
	m := groupManager(t, first, second)
	targets := []ModelGroupTarget{{Provider: "first", Model: "one"}, {Provider: "second", Model: "two"}}
	result, err := m.ExecuteStreamModelGroup(context.Background(), targets, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil || result == nil || len(attempts) != 2 {
		t.Fatalf("result = %p, error = %v, attempts = %v", result, err, attempts)
	}
	assertBootstrap(result)

	// Use a fresh manager for the second case: the first case legitimately
	// records the first credential as unavailable after its bootstrap error.
	attempts = nil
	first = &groupExecutor{provider: "first", stream: bootstrapStream(), attempts: &attempts}
	second = &groupExecutor{provider: "second", stream: bootstrapStream(), attempts: &attempts}
	m = groupManager(t, first, second)
	result, err = m.ExecuteStreamModelGroup(context.Background(), targets, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil || result == nil || len(attempts) != 1 {
		t.Fatalf("result = %p, error = %v, attempts = %v", result, err, attempts)
	}
	assertBootstrap(result)
}

func TestModelGroupFallbackErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"400", &Error{HTTPStatus: http.StatusBadRequest, Message: "bad"}, false},
		{"client 401", &Error{HTTPStatus: http.StatusUnauthorized, Code: ErrorCodeRequestScoped, Message: "client token"}, false},
		{"policy 403", &Error{HTTPStatus: http.StatusForbidden, Code: "invalid_credential_policy", Message: "policy"}, false},
		{"typed auth 401", &Error{HTTPStatus: http.StatusUnauthorized, Code: "auth_expired", Message: "expired"}, true},
		{"typed auth 403", &Error{HTTPStatus: http.StatusForbidden, Code: "auth_unavailable", Message: "unavailable"}, true},
		{"429", &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}, true},
		{"502", &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream"}, true},
		{"503", &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream"}, true},
		{"canceled", context.Canceled, false},
		{"plain EOF", io.EOF, true},
		{"wrapped EOF", fmt.Errorf("transport failed: %w", io.ErrUnexpectedEOF), true},
		{"network error", &url.Error{Op: "Post", URL: "https://upstream.invalid", Err: io.EOF}, true},
		{"code-less credential 401", &Error{HTTPStatus: http.StatusUnauthorized, Message: "credential rejected"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isModelGroupFallbackError(tt.err); got != tt.want {
				t.Fatalf("isModelGroupFallbackError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestExecuteStreamModelGroupFallsBackFromRealManagerBootstrapError(t *testing.T) {
	var attempts []string
	bootstrapChunks := make(chan cliproxyexecutor.StreamChunk, 1)
	bootstrapChunks <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "bootstrap"}}
	close(bootstrapChunks)
	successChunks := make(chan cliproxyexecutor.StreamChunk, 1)
	successChunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(successChunks)
	first := &groupExecutor{provider: "first", stream: &cliproxyexecutor.StreamResult{Chunks: bootstrapChunks}, attempts: &attempts}
	second := &groupExecutor{provider: "second", stream: &cliproxyexecutor.StreamResult{Chunks: successChunks}, attempts: &attempts}
	m := groupManager(t, first, second)
	result, err := m.ExecuteStreamModelGroup(context.Background(), []ModelGroupTarget{{Provider: "first", Model: "one"}, {Provider: "second", Model: "two"}}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	chunk := <-result.Chunks
	if string(chunk.Payload) != "ok" || len(attempts) != 2 || attempts[1] != "second/two" {
		t.Fatalf("chunk = %#v, attempts = %v", chunk, attempts)
	}
}

func TestExecuteStreamModelGroupDoesNotReplayLaterErrorChunk(t *testing.T) {
	var attempts []string
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("committed")}
	chunks <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "late"}}
	close(chunks)
	first := &groupExecutor{provider: "first", stream: &cliproxyexecutor.StreamResult{Chunks: chunks}, attempts: &attempts}
	second := &groupExecutor{provider: "second", stream: &cliproxyexecutor.StreamResult{}, attempts: &attempts}
	m := groupManager(t, first, second)
	result, err := m.ExecuteStreamModelGroup(context.Background(), []ModelGroupTarget{{Provider: "first", Model: "one"}, {Provider: "second", Model: "two"}}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	firstChunk := <-result.Chunks
	lateChunk := <-result.Chunks
	if string(firstChunk.Payload) != "committed" || lateChunk.Err == nil || len(attempts) != 1 || attempts[0] != "first/one" {
		t.Fatalf("first chunk = %#v, late error = %v, attempts = %v", firstChunk, lateChunk.Err, attempts)
	}
}
