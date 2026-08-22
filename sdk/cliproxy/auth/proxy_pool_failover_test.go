package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type failoverTransport struct {
	fail bool
}

type homePoolFailoverExecutor struct{ failoverExecutor }

func (*homePoolFailoverExecutor) Identifier() string { return openAICompatPoolProviderKey }

func TestHomeNonStreamAndCountRetainSelectionDuringProxyFailover(t *testing.T) {
	for _, countTokens := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "count"}[countTokens], func(t *testing.T) {
			dispatcher := &homePerSelectionDispatcher{auths: []Auth{{
				ID: "home-pool-auth", Provider: openAICompatPoolProviderKey, ProxyPool: "office", Status: StatusActive,
				Attributes: map[string]string{"api_key": "test", "compat_name": "pool", "provider_key": "pool"},
			}}}
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{
				Home:                internalconfig.HomeConfig{Enabled: true},
				OpenAICompatibility: []internalconfig.OpenAICompatibility{{Name: "pool", Models: []internalconfig.OpenAICompatibilityModel{{Name: "upstream", Alias: "requested"}}}},
			})
			manager.SetRoundTripperProvider(&failoverProvider{transports: []*failoverTransport{{fail: true}, {}}})
			manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
			executor := &homePoolFailoverExecutor{}
			manager.RegisterExecutor(executor)

			var resp cliproxyexecutor.Response
			var err error
			if countTokens {
				resp, err = manager.ExecuteCount(context.Background(), []string{openAICompatPoolProviderKey}, cliproxyexecutor.Request{Model: "requested"}, cliproxyexecutor.Options{})
			} else {
				resp, err = manager.Execute(context.Background(), []string{openAICompatPoolProviderKey}, cliproxyexecutor.Request{Model: "requested"}, cliproxyexecutor.Options{})
			}
			if err != nil || string(resp.Payload) != "ok" {
				t.Fatalf("response=%q err=%v", resp.Payload, err)
			}
			if dispatcher.calls.Load() != 1 || executor.executeCalls != 2 {
				t.Fatalf("dispatches=%d attempts=%d", dispatcher.calls.Load(), executor.executeCalls)
			}
		})
	}
}

func (t *failoverTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if t.fail {
		return nil, errors.New("proxy bootstrap failed")
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
}

func (t *failoverTransport) ProxyTransportFailed() bool { return t.fail }

type failoverProvider struct {
	mu         sync.Mutex
	transports []*failoverTransport
	next       int
}

func (p *failoverProvider) RoundTripperFor(*Auth) http.RoundTripper {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.next
	if idx >= len(p.transports) {
		idx = len(p.transports) - 1
	}
	p.next++
	return p.transports[idx]
}

func (p *failoverProvider) ProxyPoolAttemptLimit(*Auth) int { return len(p.transports) }

type failoverExecutor struct {
	mu             sync.Mutex
	executeCalls   int
	streamCalls    int
	payloadThenErr bool
}

func (*failoverExecutor) Identifier() string { return "test" }

func (e *failoverExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls++
	e.mu.Unlock()
	rt, _ := ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://upstream.example", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return cliproxyexecutor.Response{Payload: payload}, nil
}

func (e *failoverExecutor) ExecuteStream(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamCalls++
	payloadThenErr := e.payloadThenErr
	e.mu.Unlock()
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	if payloadThenErr {
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("payload")}
		chunks <- cliproxyexecutor.StreamChunk{Err: errors.New("after payload")}
		close(chunks)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	rt, _ := ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://upstream.example", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		chunks <- cliproxyexecutor.StreamChunk{Err: err}
		close(chunks)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	resp.Body.Close()
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("payload")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (*failoverExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }
func (e *failoverExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func TestCountTokensWithProxyFailoverRetriesSameAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRoundTripperProvider(&failoverProvider{transports: []*failoverTransport{{fail: true}, {}}})
	executor := &failoverExecutor{}
	auth := &Auth{ID: "auth", ProxyPool: "office"}
	resp, err := manager.countTokensWithProxyFailover(context.Background(), executor, auth, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil || string(resp.Payload) != "ok" || executor.executeCalls != 2 {
		t.Fatalf("response=%q calls=%d err=%v", resp.Payload, executor.executeCalls, err)
	}
}
func (*failoverExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestExecuteWithProxyFailoverRetriesSameAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRoundTripperProvider(&failoverProvider{transports: []*failoverTransport{{fail: true}, {}}})
	executor := &failoverExecutor{}
	auth := &Auth{ID: "auth", ProxyPool: "office"}
	resp, err := manager.executeWithProxyFailover(context.Background(), executor, auth, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil || string(resp.Payload) != "ok" || executor.executeCalls != 2 {
		t.Fatalf("response=%q calls=%d err=%v", resp.Payload, executor.executeCalls, err)
	}
}

func TestExecuteStreamWithProxyFailoverRetriesBeforePayloadOnly(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	provider := &failoverProvider{transports: []*failoverTransport{{fail: true}, {}}}
	manager.SetRoundTripperProvider(provider)
	executor := &failoverExecutor{}
	auth := &Auth{ID: "auth", ProxyPool: "office"}
	result, err := manager.executeStreamWithModelPool(context.Background(), executor, auth, "test", cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, "model", "", []string{"model"}, false, OAuthModelAliasResult{}, nil, true, false, nil)
	if err != nil || executor.streamCalls != 2 {
		t.Fatalf("calls=%d err=%v", executor.streamCalls, err)
	}
	chunk := <-result.Chunks
	if string(chunk.Payload) != "payload" {
		t.Fatalf("payload=%q", chunk.Payload)
	}

	executor = &failoverExecutor{payloadThenErr: true}
	provider.next = 0
	result, err = manager.executeStreamWithModelPool(context.Background(), executor, auth, "test", cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, "model", "", []string{"model"}, false, OAuthModelAliasResult{}, nil, true, false, nil)
	if err != nil || executor.streamCalls != 1 {
		t.Fatalf("payload-then-error calls=%d err=%v", executor.streamCalls, err)
	}
	chunk = <-result.Chunks
	if string(chunk.Payload) != "payload" {
		t.Fatalf("payload=%q", chunk.Payload)
	}
}
