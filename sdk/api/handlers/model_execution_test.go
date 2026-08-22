package handlers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/contextcompression"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type modelExecutionCaptureExecutor struct {
	provider string

	mu          sync.Mutex
	lastRequest coreexecutor.Request
	lastOptions coreexecutor.Options
	execute     func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error)
	stream      func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error)
}

type compressionRetryExecutor struct {
	mu           sync.Mutex
	provider     string
	payloads     [][]byte
	failFirst    bool
	useTransport bool
}

func (e *compressionRetryExecutor) Identifier() string { return e.provider }
func (e *compressionRetryExecutor) Execute(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, cloneBytes(req.Payload))
	call := len(e.payloads)
	e.mu.Unlock()
	if e.failFirst && call == 1 {
		// Match the manager's immediate same-round failover fixtures: a provider
		// failure with cooling disabled advances to the next credential without
		// entering refresh or delayed retry handling.
		return coreexecutor.Response{}, &coreauth.Error{HTTPStatus: http.StatusInternalServerError, Message: "retry"}
	}
	if e.useTransport {
		rt, _ := ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://upstream.example", nil)
		resp, err := rt.RoundTrip(request)
		if err != nil {
			return coreexecutor.Response{}, err
		}
		_ = resp.Body.Close()
	}
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *compressionRetryExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, fmt.Errorf("unused")
}
func (e *compressionRetryExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}
func (e *compressionRetryExecutor) CountTokens(ctx context.Context, a *coreauth.Auth, r coreexecutor.Request, o coreexecutor.Options) (coreexecutor.Response, error) {
	return e.Execute(ctx, a, r, o)
}
func (e *compressionRetryExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unused")
}

type compressionFailTransport struct{ fail bool }

func (t *compressionFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if t.fail {
		return nil, fmt.Errorf("proxy failed")
	}
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
}
func (t *compressionFailTransport) ProxyTransportFailed() bool { return t.fail }

type compressionTransportProvider struct {
	mu         sync.Mutex
	next       int
	transports []*compressionFailTransport
}

func (p *compressionTransportProvider) RoundTripperFor(*coreauth.Auth) http.RoundTripper {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.next
	if i >= len(p.transports) {
		i = len(p.transports) - 1
	}
	p.next++
	return p.transports[i]
}
func (p *compressionTransportProvider) ProxyPoolAttemptLimit(*coreauth.Auth) int {
	return len(p.transports)
}

type modelGroupCaptureExecutor struct {
	provider string
	req      coreexecutor.Request
	opts     coreexecutor.Options
	count    int
	stream   func(coreexecutor.Request, coreexecutor.Options, int) (*coreexecutor.StreamResult, error)
}

func (e *modelGroupCaptureExecutor) Identifier() string { return e.provider }
func (e *modelGroupCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.req, e.opts, e.count = req, opts, e.count+1
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *modelGroupCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.req, e.opts, e.count = req, opts, e.count+1
	if e.stream != nil {
		return e.stream(req, opts, e.count)
	}
	return nil, &coreauth.Error{HTTPStatus: http.StatusNotImplemented, Message: "unused"}
}
func (e *modelGroupCaptureExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}
func (e *modelGroupCaptureExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.req, e.opts, e.count = req, opts, e.count+1
	return coreexecutor.Response{Payload: []byte("1")}, nil
}
func (*modelGroupCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type modelExecutionStatusHeaderError struct {
	statusCode int
	message    string
	headers    http.Header
}

type modelExecutionSkipHost struct {
	beforeSkip string
	afterSkip  string
	respSkip   string
	streamSkip []string
}

func (h *modelExecutionSkipHost) InterceptRequestBeforeAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	panic("InterceptRequestBeforeAuth called without skip")
}

func (h *modelExecutionSkipHost) InterceptRequestAfterAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	panic("InterceptRequestAfterAuth called without skip")
}

func (h *modelExecutionSkipHost) InterceptResponse(context.Context, pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	panic("InterceptResponse called without skip")
}

func (h *modelExecutionSkipHost) InterceptStreamChunk(context.Context, pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	panic("InterceptStreamChunk called without skip")
}

func (h *modelExecutionSkipHost) InterceptRequestBeforeAuthExcept(ctx context.Context, req pluginapi.RequestInterceptRequest, skipPluginID string) pluginapi.RequestInterceptResponse {
	h.beforeSkip = skipPluginID
	return pluginapi.RequestInterceptResponse{
		Headers: cloneHeader(req.Headers),
		Body:    cloneBytes(req.Body),
	}
}

func (h *modelExecutionSkipHost) InterceptRequestAfterAuthExcept(ctx context.Context, req pluginapi.RequestInterceptRequest, skipPluginID string) pluginapi.RequestInterceptResponse {
	h.afterSkip = skipPluginID
	return pluginapi.RequestInterceptResponse{
		Headers: cloneHeader(req.Headers),
		Body:    cloneBytes(req.Body),
	}
}

func (h *modelExecutionSkipHost) InterceptResponseExcept(ctx context.Context, req pluginapi.ResponseInterceptRequest, skipPluginID string) pluginapi.ResponseInterceptResponse {
	h.respSkip = skipPluginID
	return pluginapi.ResponseInterceptResponse{
		Headers: cloneHeader(req.ResponseHeaders),
		Body:    cloneBytes(req.Body),
	}
}

func (h *modelExecutionSkipHost) InterceptStreamChunkExcept(ctx context.Context, req pluginapi.StreamChunkInterceptRequest, skipPluginID string) pluginapi.StreamChunkInterceptResponse {
	h.streamSkip = append(h.streamSkip, skipPluginID)
	return pluginapi.StreamChunkInterceptResponse{
		Headers: cloneHeader(req.ResponseHeaders),
		Body:    cloneBytes(req.Body),
	}
}

func (e modelExecutionStatusHeaderError) Error() string {
	return e.message
}

func (e modelExecutionStatusHeaderError) StatusCode() int {
	return e.statusCode
}

func (e modelExecutionStatusHeaderError) Headers() http.Header {
	return e.headers
}

func (e *modelExecutionCaptureExecutor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "codex"
}

func (e *modelExecutionCaptureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.capture(req, opts)
	if e.execute != nil {
		return e.execute(ctx, auth, req, opts)
	}
	return coreexecutor.Response{Payload: []byte("model-execution-ok")}, nil
}

func (e *modelExecutionCaptureExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.capture(req, opts)
	if e.stream != nil {
		return e.stream(ctx, auth, req, opts)
	}
	chunks := make(chan coreexecutor.StreamChunk)
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *modelExecutionCaptureExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *modelExecutionCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte("0")}, nil
}

func (e *modelExecutionCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *modelExecutionCaptureExecutor) capture(req coreexecutor.Request, opts coreexecutor.Options) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastRequest = coreexecutor.Request{
		Model:    req.Model,
		Payload:  cloneBytes(req.Payload),
		Format:   req.Format,
		Metadata: req.Metadata,
	}
	e.lastOptions = coreexecutor.Options{
		Stream:          opts.Stream,
		Alt:             opts.Alt,
		Headers:         cloneHeader(opts.Headers),
		Query:           cloneURLValues(opts.Query),
		OriginalRequest: cloneBytes(opts.OriginalRequest),
		SourceFormat:    opts.SourceFormat,
		ResponseFormat:  opts.ResponseFormat,
		Metadata:        opts.Metadata,
	}
}

func (e *modelExecutionCaptureExecutor) captured() (coreexecutor.Request, coreexecutor.Options) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastRequest, e.lastOptions
}

func newModelExecutionHandler(t *testing.T, model string, executor *modelExecutionCaptureExecutor, cfg *sdkconfig.SDKConfig) *BaseAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "model-execution-" + model,
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": model + "@example.com"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("manager.Register(): %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	return NewBaseAPIHandlers(cfg, manager)
}

func TestModelGroupRewritesTargetAndPreservesRequestedModelMetadata(t *testing.T) {
	first := &modelGroupCaptureExecutor{provider: "group-first"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(first)
	registry.GetGlobalRegistry().RegisterClient("group-first", "group-first", []*registry.ModelInfo{{ID: "upstream-one"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("group-first") })
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "group-first", Provider: "group-first", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{"upstream-one": {Status: coreauth.StatusActive}}}); err != nil {
		t.Fatal(err)
	}
	cfg := &sdkconfig.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "code-auto", Models: []config.ModelGroupMember{{Provider: "group-first", Model: "upstream-one"}}}}}
	handler := NewBaseAPIHandlers(cfg, manager)
	body, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "code-auto", []byte(`{"model":"code-auto"}`), "")
	if errMsg != nil || string(body) != `{"ok":true}` {
		t.Fatalf("body = %s, error = %+v", body, errMsg)
	}
	if first.req.Model != "upstream-one" || first.opts.Metadata[coreexecutor.RequestedModelMetadataKey] != "code-auto" {
		t.Fatalf("request = %#v, metadata = %#v", first.req, first.opts.Metadata)
	}
}

func TestModelGroupBootstrapRetryReusesCompressedPayloadExactlyOnce(t *testing.T) {
	originalApply := applyInlineContextCompression
	calls := 0
	applyInlineContextCompression = func(runtime *contextcompression.Runtime, ctx context.Context, raw []byte, cfg config.ContextCompressionConfig, optOut bool) ([]byte, contextcompression.Stats) {
		calls++
		return runtime.Apply(ctx, raw, cfg, optOut)
	}
	t.Cleanup(func() { applyInlineContextCompression = originalApply })
	var payloads [][]byte
	executor := &modelGroupCaptureExecutor{provider: "group-stream"}
	executor.stream = func(req coreexecutor.Request, _ coreexecutor.Options, call int) (*coreexecutor.StreamResult, error) {
		payloads = append(payloads, cloneBytes(req.Payload))
		chunks := make(chan coreexecutor.StreamChunk, 1)
		if call == 1 {
			chunks <- coreexecutor.StreamChunk{Err: &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}}
		} else {
			chunks <- coreexecutor.StreamChunk{Payload: []byte("ok")}
		}
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	for _, id := range []string{"group-stream-1", "group-stream-2"} {
		auth := &coreauth.Auth{ID: id, Provider: "group-stream", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{"upstream": {Status: coreauth.StatusActive}}}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "group-stream", []*registry.ModelInfo{{ID: "upstream"}})
		defer registry.GetGlobalRegistry().UnregisterClient(id)
	}
	cfg := &sdkconfig.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "group-auto", Models: []config.ModelGroupMember{{Provider: "group-stream", Model: "upstream"}}}}, Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 1}, ContextCompression: config.ContextCompressionConfig{Engine: config.ContextCompressionRTK, MinBytes: 1, RawCapBytes: 1024 * 1024}}
	raw := []byte(`{"model":"group-auto","messages":[{"role":"tool","content":"same repeated historical output line\nsame repeated historical output line\nsame repeated historical output line\nsame repeated historical output line\nsame repeated historical output line\nsame repeated historical output line"}]}`)
	data, _, errs := NewBaseAPIHandlers(cfg, manager).ExecuteStreamWithAuthManager(context.Background(), "openai", "group-auto", raw, "")
	for range data {
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || len(payloads) != 2 || !bytes.Equal(payloads[0], payloads[1]) || bytes.Equal(payloads[0], raw) {
		t.Fatalf("calls=%d payloads=%q", calls, payloads)
	}
}

func TestCompressionExactlyOnceAcrossCredentialAndProxyFallback(t *testing.T) {
	for _, tc := range []struct {
		name              string
		credential, proxy bool
	}{{"credential", true, false}, {"proxy", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			original := applyInlineContextCompression
			calls := 0
			applyInlineContextCompression = func(runtime *contextcompression.Runtime, ctx context.Context, raw []byte, cfg config.ContextCompressionConfig, opt bool) ([]byte, contextcompression.Stats) {
				calls++
				return runtime.Apply(ctx, raw, cfg, opt)
			}
			t.Cleanup(func() { applyInlineContextCompression = original })
			provider := "compression-" + tc.name
			executor := &compressionRetryExecutor{provider: provider, failFirst: tc.credential, useTransport: tc.proxy}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			if tc.proxy {
				manager.SetRoundTripperProvider(&compressionTransportProvider{transports: []*compressionFailTransport{{fail: true}, {}}})
			}
			authCount := 1
			if tc.credential {
				authCount = 2
			}
			for i := 0; i < authCount; i++ {
				id := fmt.Sprintf("%s-%d", provider, i)
				auth := &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive, ProxyPool: map[bool]string{true: "office"}[tc.proxy], Metadata: map[string]any{"request_retry": 0, "disable_cooling": true}}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: "model"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			cfg := &sdkconfig.SDKConfig{ContextCompression: config.ContextCompressionConfig{Engine: config.ContextCompressionRTK, MinBytes: 1, RawCapBytes: 1024 * 1024}}
			raw := []byte(`{"model":"model","messages":[{"role":"tool","content":"same repeated output line\nsame repeated output line\nsame repeated output line\nsame repeated output line\nsame repeated output line\nsame repeated output line"}]}`)
			execCtx, cancelExec := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelExec()
			body, _, errMsg := NewBaseAPIHandlers(cfg, manager).ExecuteWithAuthManager(execCtx, "openai", "model", raw, "")
			if errMsg != nil || len(body) == 0 {
				t.Fatalf("body=%s err=%+v", body, errMsg)
			}
			if calls != 1 || len(executor.payloads) != 2 || !bytes.Equal(executor.payloads[0], executor.payloads[1]) || bytes.Equal(executor.payloads[0], raw) {
				t.Fatalf("calls=%d payloads=%q", calls, executor.payloads)
			}
		})
	}
}

func TestModelGroupCountUsesTargetModel(t *testing.T) {
	first := &modelGroupCaptureExecutor{provider: "group-count"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(first)
	registry.GetGlobalRegistry().RegisterClient("group-count", "group-count", []*registry.ModelInfo{{ID: "count-upstream"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("group-count") })
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "group-count", Provider: "group-count", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{"count-upstream": {Status: coreauth.StatusActive}}}); err != nil {
		t.Fatal(err)
	}
	cfg := &sdkconfig.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "count-auto", Models: []config.ModelGroupMember{{Provider: "group-count", Model: "count-upstream"}}}}}
	_, _, errMsg := NewBaseAPIHandlers(cfg, manager).ExecuteCountWithAuthManager(context.Background(), "claude", "count-auto", []byte(`{"model":"count-auto"}`), "")
	if errMsg != nil || first.req.Model != "count-upstream" || first.opts.Metadata[coreexecutor.RequestedModelMetadataKey] != "count-auto" {
		t.Fatalf("request = %#v, metadata = %#v, error = %+v", first.req, first.opts.Metadata, errMsg)
	}
}

func TestModelGroupRejectsImageAndForcedProviderPaths(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "group", Models: []config.ModelGroupMember{{Provider: "codex", Model: "gpt-test"}}}}}, nil)
	_, _, imageErr := handler.providersForExecution("group", "group", true, modelRouteDecision{}, modelExecutionOptions{})
	if imageErr == nil || imageErr.StatusCode != http.StatusBadRequest || !strings.Contains(imageErr.Error.Error(), "image endpoints") {
		t.Fatalf("image error = %+v", imageErr)
	}
	_, _, forcedErr := handler.providersForExecution("group", "group", false, modelRouteDecision{}, modelExecutionOptions{ForcedProvider: "codex"})
	if forcedErr == nil || forcedErr.StatusCode != http.StatusBadRequest || !strings.Contains(forcedErr.Error.Error(), "forced-provider") {
		t.Fatalf("forced-provider error = %+v", forcedErr)
	}
}

func TestModelGroupRejectsAnyImageOnlyMemberOnNonImageEndpoint(t *testing.T) {
	imageModel := "gpt-image-2"
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "mixed", Models: []config.ModelGroupMember{{Provider: "codex", Model: "text"}, {Provider: "compat", Model: imageModel}}}}}, nil)
	_, _, errMsg := handler.providersForExecution("mixed", "mixed", false, modelRouteDecision{}, modelExecutionOptions{})
	if errMsg == nil || errMsg.StatusCode != http.StatusServiceUnavailable || !strings.Contains(errMsg.Error.Error(), imageModel) {
		t.Fatalf("error = %+v", errMsg)
	}
}

func TestModelGroupListingsUseProtocolNativeShapes(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "code-auto", Models: []config.ModelGroupMember{{Provider: "codex", Model: "gpt-test"}}}}}, nil)
	claude := handler.ModelGroupModels("claude")[0]
	if claude["id"] != "code-auto" || claude["type"] != "model" || claude["owned_by"] != "ainyrouter/model-group" {
		t.Fatalf("Claude model = %#v", claude)
	}
	gemini := handler.ModelGroupModels("gemini")[0]
	if gemini["name"] != "code-auto" || gemini["displayName"] != "code-auto" {
		t.Fatalf("Gemini model = %#v", gemini)
	}
}

func TestModelGroupTerminalErrorDoesNotClaimFirstTarget(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{ModelGroups: []config.ModelGroup{{Name: "code-auto", Models: []config.ModelGroupMember{{Provider: "first-provider", Model: "first-model"}, {Provider: "second-provider", Model: "second-model"}}}}}, manager)
	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "code-auto", nil, "")
	if errMsg == nil || errMsg.Error == nil {
		t.Fatal("expected terminal group error")
	}
	if strings.Contains(errMsg.Error.Error(), "first-provider") || strings.Contains(errMsg.Error.Error(), "first-model") {
		t.Fatalf("terminal error contains stale first target: %v", errMsg.Error)
	}
}

func TestExecuteModelCarriesEntryAndExitProtocols(t *testing.T) {
	model := "model-execution-nonstream-model"
	requestBody := []byte(fmt.Sprintf(`{"model":%q}`, model))
	executor := &modelExecutionCaptureExecutor{
		execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			return coreexecutor.Response{
				Payload: []byte(`{"ok":true}`),
				Headers: http.Header{
					"X-Upstream": []string{"nonstream"},
				},
			}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{PassthroughHeaders: true})

	resp, errMsg := handler.ExecuteModel(context.Background(), ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "claude",
		Model:         model,
		Body:          requestBody,
		Headers:       http.Header{"X-Callback": []string{"nonstream"}},
		Query:         url.Values{"q": []string{"callback"}},
	})
	if errMsg != nil {
		t.Fatalf("ExecuteModel() error = %+v", errMsg)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Fatalf("body = %q, want executor response", resp.Body)
	}
	if resp.Headers.Get("X-Upstream") != "nonstream" {
		t.Fatalf("headers = %#v, want upstream header", resp.Headers)
	}

	gotReq, gotOpts := executor.captured()
	if gotReq.Model != model {
		t.Fatalf("executor model = %q, want %q", gotReq.Model, model)
	}
	if string(gotReq.Payload) != string(requestBody) {
		t.Fatalf("executor payload = %q, want %q", gotReq.Payload, requestBody)
	}
	if gotOpts.Stream {
		t.Fatal("executor stream option = true, want false")
	}
	if gotOpts.SourceFormat != sdktranslator.FormatOpenAI {
		t.Fatalf("SourceFormat = %q, want %q", gotOpts.SourceFormat, sdktranslator.FormatOpenAI)
	}
	if gotOpts.ResponseFormat != sdktranslator.FormatClaude {
		t.Fatalf("ResponseFormat = %q, want %q", gotOpts.ResponseFormat, sdktranslator.FormatClaude)
	}
	if gotOpts.Metadata[coreexecutor.RequestedModelMetadataKey] != model {
		t.Fatalf("requested model metadata = %#v, want %q", gotOpts.Metadata[coreexecutor.RequestedModelMetadataKey], model)
	}
	if gotOpts.Metadata[modelExecutionMetadataSourceKey] != modelExecutionInternalSource {
		t.Fatalf("source metadata = %#v, want %q", gotOpts.Metadata[modelExecutionMetadataSourceKey], modelExecutionInternalSource)
	}
	if gotOpts.Headers.Get("X-Callback") != "nonstream" {
		t.Fatalf("executor headers = %#v, want callback header", gotOpts.Headers)
	}
	if gotOpts.Query.Get("q") != "callback" {
		t.Fatalf("executor query = %#v, want callback query", gotOpts.Query)
	}
}

func TestExecuteModelSkipsOriginatingPluginInterceptors(t *testing.T) {
	model := "model-execution-skip-origin-model"
	requestBody := []byte(fmt.Sprintf(`{"model":%q}`, model))
	executor := &modelExecutionCaptureExecutor{}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})
	skipHost := &modelExecutionSkipHost{}
	handler.SetPluginHost(skipHost)

	resp, errMsg := handler.ExecuteModel(context.Background(), ModelExecutionRequest{
		EntryProtocol:           "openai",
		ExitProtocol:            "openai",
		Model:                   model,
		Body:                    requestBody,
		SkipInterceptorPluginID: "origin-plugin",
	})
	if errMsg != nil {
		t.Fatalf("ExecuteModel() error = %+v", errMsg)
	}
	if string(resp.Body) != "model-execution-ok" {
		t.Fatalf("body = %q, want executor response", resp.Body)
	}
	if skipHost.beforeSkip != "origin-plugin" || skipHost.afterSkip != "origin-plugin" || skipHost.respSkip != "origin-plugin" {
		t.Fatalf("skip ids = before:%q after:%q response:%q, want origin-plugin", skipHost.beforeSkip, skipHost.afterSkip, skipHost.respSkip)
	}
}

func TestExecuteModelStream(t *testing.T) {
	model := "model-execution-stream-model"
	requestBody := []byte(fmt.Sprintf(`{"model":%q,"stream":true}`, model))
	executor := &modelExecutionCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("stream-one")}
			close(chunks)
			return &coreexecutor.StreamResult{
				Headers: http.Header{"X-Upstream": []string{"stream"}},
				Chunks:  chunks,
			}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{PassthroughHeaders: true})

	stream, errMsg := handler.ExecuteModelStream(context.Background(), ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "claude",
		Model:         model,
		Stream:        true,
		Body:          requestBody,
		Headers:       http.Header{"X-Callback": []string{"stream"}},
	})
	if errMsg != nil {
		t.Fatalf("ExecuteModelStream() error = %+v", errMsg)
	}
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", stream.StatusCode, http.StatusOK)
	}
	if stream.Headers.Get("X-Upstream") != "stream" {
		t.Fatalf("headers = %#v, want upstream header", stream.Headers)
	}
	chunk, ok := <-stream.Chunks
	if !ok {
		t.Fatal("stream chunks closed before payload")
	}
	if chunk.Err != nil {
		t.Fatalf("stream chunk error = %+v", chunk.Err)
	}
	if string(chunk.Payload) != "stream-one" {
		t.Fatalf("stream chunk payload = %q, want stream-one", chunk.Payload)
	}
	if chunk, ok = <-stream.Chunks; ok {
		t.Fatalf("unexpected extra stream chunk: %+v", chunk)
	}

	gotReq, gotOpts := executor.captured()
	if gotReq.Model != model {
		t.Fatalf("executor model = %q, want %q", gotReq.Model, model)
	}
	if string(gotReq.Payload) != string(requestBody) {
		t.Fatalf("executor payload = %q, want %q", gotReq.Payload, requestBody)
	}
	if !gotOpts.Stream {
		t.Fatal("executor stream option = false, want true")
	}
	if gotOpts.SourceFormat != sdktranslator.FormatOpenAI {
		t.Fatalf("SourceFormat = %q, want %q", gotOpts.SourceFormat, sdktranslator.FormatOpenAI)
	}
	if gotOpts.ResponseFormat != sdktranslator.FormatClaude {
		t.Fatalf("ResponseFormat = %q, want %q", gotOpts.ResponseFormat, sdktranslator.FormatClaude)
	}
	if gotOpts.Metadata[coreexecutor.RequestedModelMetadataKey] != model {
		t.Fatalf("requested model metadata = %#v, want %q", gotOpts.Metadata[coreexecutor.RequestedModelMetadataKey], model)
	}
	if gotOpts.Metadata[modelExecutionMetadataSourceKey] != modelExecutionInternalSource {
		t.Fatalf("source metadata = %#v, want %q", gotOpts.Metadata[modelExecutionMetadataSourceKey], modelExecutionInternalSource)
	}
	if gotOpts.Headers.Get("X-Callback") != "stream" {
		t.Fatalf("executor headers = %#v, want callback header", gotOpts.Headers)
	}
}

func TestExecuteModelStreamSkipsOriginatingPluginInterceptors(t *testing.T) {
	model := "model-execution-stream-skip-origin-model"
	requestBody := []byte(fmt.Sprintf(`{"model":%q,"stream":true}`, model))
	executor := &modelExecutionCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("stream-one")}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})
	skipHost := &modelExecutionSkipHost{}
	handler.SetPluginHost(skipHost)

	stream, errMsg := handler.ExecuteModelStream(context.Background(), ModelExecutionRequest{
		EntryProtocol:           "openai",
		ExitProtocol:            "openai",
		Model:                   model,
		Stream:                  true,
		Body:                    requestBody,
		SkipInterceptorPluginID: "origin-plugin",
	})
	if errMsg != nil {
		t.Fatalf("ExecuteModelStream() error = %+v", errMsg)
	}
	chunk, ok := <-stream.Chunks
	if !ok {
		t.Fatal("stream chunks closed before payload")
	}
	if string(chunk.Payload) != "stream-one" {
		t.Fatalf("stream chunk payload = %q, want stream-one", chunk.Payload)
	}
	if skipHost.beforeSkip != "origin-plugin" || skipHost.afterSkip != "origin-plugin" {
		t.Fatalf("request skip ids = before:%q after:%q, want origin-plugin", skipHost.beforeSkip, skipHost.afterSkip)
	}
	if len(skipHost.streamSkip) == 0 {
		t.Fatal("stream interceptor was not called with skip")
	}
	for _, skipID := range skipHost.streamSkip {
		if skipID != "origin-plugin" {
			t.Fatalf("stream skip id = %q, want origin-plugin", skipID)
		}
	}
}

func TestExecuteModelStreamStartupError(t *testing.T) {
	model := "model-execution-stream-startup-error-model"
	requestBody := []byte(fmt.Sprintf(`{"model":%q,"stream":true}`, model))
	executor := &modelExecutionCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Err: fmt.Errorf("startup failed")}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})

	stream, errMsg := handler.ExecuteModelStream(context.Background(), ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "claude",
		Model:         model,
		Stream:        true,
		Body:          requestBody,
	})
	if errMsg == nil {
		t.Fatal("ExecuteModelStream() error = nil, want startup error")
	}
	if errMsg.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusInternalServerError)
	}
	if errMsg.Error == nil || errMsg.Error.Error() != "startup failed" {
		t.Fatalf("error = %v, want startup failed", errMsg.Error)
	}
	if stream.Chunks != nil {
		t.Fatal("stream chunks created for startup error")
	}
}

func TestExecuteModelStreamTerminalError(t *testing.T) {
	model := "model-execution-stream-terminal-error-model"
	requestBody := []byte(fmt.Sprintf(`{"model":%q,"stream":true}`, model))
	errorHeaders := http.Header{"X-Stream-Error": []string{"terminal"}}
	executor := &modelExecutionCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 2)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("stream-before-error")}
			chunks <- coreexecutor.StreamChunk{Err: modelExecutionStatusHeaderError{
				statusCode: http.StatusTooManyRequests,
				message:    "rate limited",
				headers:    errorHeaders,
			}}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})

	stream, errMsg := handler.ExecuteModelStream(context.Background(), ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "claude",
		Model:         model,
		Stream:        true,
		Body:          requestBody,
	})
	if errMsg != nil {
		t.Fatalf("ExecuteModelStream() error = %+v", errMsg)
	}

	chunk, ok := <-stream.Chunks
	if !ok {
		t.Fatal("stream chunks closed before payload")
	}
	if chunk.Err != nil {
		t.Fatalf("first stream chunk error = %+v", chunk.Err)
	}
	if string(chunk.Payload) != "stream-before-error" {
		t.Fatalf("first stream chunk payload = %q, want stream-before-error", chunk.Payload)
	}

	chunk, ok = <-stream.Chunks
	if !ok {
		t.Fatal("stream chunks closed before terminal error")
	}
	if len(chunk.Payload) != 0 {
		t.Fatalf("terminal stream chunk payload = %q, want empty", chunk.Payload)
	}
	if chunk.Err == nil {
		t.Fatal("terminal stream chunk error = nil")
	}
	if chunk.Err.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("terminal status = %d, want %d", chunk.Err.StatusCode, http.StatusTooManyRequests)
	}
	if chunk.Err.Message != "rate limited" {
		t.Fatalf("terminal message = %q, want rate limited", chunk.Err.Message)
	}
	if chunk.Err.Error() != "rate limited" {
		t.Fatalf("terminal Error() = %q, want rate limited", chunk.Err.Error())
	}
	if chunk.Err.Headers.Get("X-Stream-Error") != "terminal" {
		t.Fatalf("terminal headers = %#v, want stream error header", chunk.Err.Headers)
	}
	if chunk, ok = <-stream.Chunks; ok {
		t.Fatalf("unexpected extra stream chunk: %+v", chunk)
	}
}

func TestExecuteModelStreamContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage)
	chunks := wrapModelExecutionChunks(ctx, dataChan, errChan, nil)

	cancel()

	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	select {
	case chunk, ok := <-chunks:
		if ok {
			t.Fatalf("stream chunks yielded after cancel: %+v", chunk)
		}
	case <-timeout.C:
		t.Fatal("stream chunks did not close after context cancellation")
	}
}

func TestExecuteProtocolWithAuthManagerUsesForcedProvider(t *testing.T) {
	model := "interactions-agent-target"
	requestBody := []byte(`{"agent":"agents/test-agent","input":"hi"}`)
	executor := &modelExecutionCaptureExecutor{
		provider: "gemini",
		execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			return coreexecutor.Response{Payload: []byte(`{"id":"interaction_1"}`)}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})

	resp, errMsg := handler.ExecuteProtocolWithAuthManager(context.Background(), ProtocolExecutionRequest{
		EntryProtocol:  "interactions",
		ExitProtocol:   "interactions",
		ForcedProvider: "gemini",
		Model:          model,
		Body:           requestBody,
	})
	if errMsg != nil {
		t.Fatalf("ExecuteProtocolWithAuthManager() error = %+v", errMsg)
	}
	if string(resp.Body) != `{"id":"interaction_1"}` {
		t.Fatalf("body = %q, want native interactions response", resp.Body)
	}

	gotReq, gotOpts := executor.captured()
	if gotReq.Model != model {
		t.Fatalf("executor model = %q, want %q", gotReq.Model, model)
	}
	if gotOpts.SourceFormat != sdktranslator.FormatInteractions {
		t.Fatalf("SourceFormat = %q, want %q", gotOpts.SourceFormat, sdktranslator.FormatInteractions)
	}
	if gotOpts.ResponseFormat != sdktranslator.FormatInteractions {
		t.Fatalf("ResponseFormat = %q, want %q", gotOpts.ResponseFormat, sdktranslator.FormatInteractions)
	}
	if gotOpts.Metadata[coreexecutor.RequestedModelMetadataKey] != model {
		t.Fatalf("requested model metadata = %#v, want %q", gotOpts.Metadata[coreexecutor.RequestedModelMetadataKey], model)
	}
}

func TestPreferExecutionProviderMovesPreferredFirst(t *testing.T) {
	providers := preferExecutionProvider([]string{"gemini", "gemini-interactions", "claude"}, "gemini-interactions")
	want := []string{"gemini-interactions", "gemini", "claude"}
	if len(providers) != len(want) {
		t.Fatalf("providers = %#v, want %#v", providers, want)
	}
	for i := range want {
		if providers[i] != want[i] {
			t.Fatalf("providers = %#v, want %#v", providers, want)
		}
	}
}

func TestAdjustExecutionProvidersExcludesInteractionsProviderForUnsupportedEntry(t *testing.T) {
	providers := adjustExecutionProvidersForEntryProtocol("codex", []string{"gemini-interactions", "codex"})
	want := []string{"codex"}
	if len(providers) != len(want) {
		t.Fatalf("providers = %#v, want %#v", providers, want)
	}
	for i := range want {
		if providers[i] != want[i] {
			t.Fatalf("providers = %#v, want %#v", providers, want)
		}
	}
}

func TestAdjustExecutionProvidersKeepsInteractionsProviderForSupportedNativeInteractionsEntries(t *testing.T) {
	for _, entryProtocol := range []string{constant.OpenAI, constant.OpenaiResponse, constant.Claude, constant.Gemini} {
		t.Run(entryProtocol, func(t *testing.T) {
			providers := adjustExecutionProvidersForEntryProtocol(entryProtocol, []string{"gemini-interactions"})
			want := []string{"gemini-interactions"}
			if len(providers) != len(want) {
				t.Fatalf("providers = %#v, want %#v", providers, want)
			}
			for i := range want {
				if providers[i] != want[i] {
					t.Fatalf("providers = %#v, want %#v", providers, want)
				}
			}
		})
	}
}

func TestExecuteModelStreamKeepsInteractionsProviderForOpenAIEntry(t *testing.T) {
	model := "gemini-3.1-flash-lite"
	requestBody := []byte(`{"model":"gemini-3.1-flash-lite","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	executor := &modelExecutionCaptureExecutor{
		provider: constant.GeminiInteractions,
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"id":"chunk_1","object":"chat.completion.chunk","choices":[]}`)}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})

	stream, errMsg := handler.ExecuteModelStream(context.Background(), ModelExecutionRequest{
		EntryProtocol: constant.OpenAI,
		ExitProtocol:  constant.OpenAI,
		Model:         model,
		Stream:        true,
		Body:          requestBody,
	})
	if errMsg != nil {
		t.Fatalf("ExecuteModelStream() error = %+v", errMsg)
	}
	for range stream.Chunks {
	}
	gotReq, gotOpts := executor.captured()
	if gotReq.Model != model {
		t.Fatalf("executor model = %q, want %q", gotReq.Model, model)
	}
	if gotOpts.SourceFormat != sdktranslator.FormatOpenAI {
		t.Fatalf("SourceFormat = %q, want %q", gotOpts.SourceFormat, sdktranslator.FormatOpenAI)
	}
}

func TestExecuteProtocolWithAuthManagerAgentUsesSelectionModelForAuth(t *testing.T) {
	selectionModel := "gemini-2.5-flash"
	agentModel := "agents/test-agent"
	requestBody := []byte(`{"agent":"agents/test-agent","input":"hi"}`)
	executor := &modelExecutionCaptureExecutor{
		provider: constant.GeminiInteractions,
		execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			return coreexecutor.Response{Payload: []byte(`{"id":"interaction_1"}`)}, nil
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "model-execution-agent-selection",
		Provider: constant.GeminiInteractions,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "agent-selection@example.com"},
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: selectionModel}, {ID: agentModel}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("manager.Register(): %v", errRegister)
	}
	manager.RefreshSchedulerEntry(auth.ID)
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

	resp, errMsg := handler.ExecuteProtocolWithAuthManager(context.Background(), ProtocolExecutionRequest{
		EntryProtocol:      "interactions",
		ExitProtocol:       "interactions",
		ForcedProvider:     constant.GeminiInteractions,
		AuthSelectionModel: selectionModel,
		Model:              agentModel,
		Body:               requestBody,
	})
	if errMsg != nil {
		t.Fatalf("ExecuteProtocolWithAuthManager() error = %+v", errMsg)
	}
	if string(resp.Body) != `{"id":"interaction_1"}` {
		t.Fatalf("body = %q, want native interactions response", resp.Body)
	}
	gotReq, gotOpts := executor.captured()
	if gotReq.Model != agentModel {
		t.Fatalf("executor model = %q, want %q", gotReq.Model, agentModel)
	}
	if string(gotReq.Payload) != string(requestBody) {
		t.Fatalf("executor payload = %q, want %q", gotReq.Payload, requestBody)
	}
	if gotOpts.Metadata[coreexecutor.AuthSelectionModelMetadataKey] != selectionModel {
		t.Fatalf("auth selection metadata = %#v, want %q", gotOpts.Metadata[coreexecutor.AuthSelectionModelMetadataKey], selectionModel)
	}
}

func TestExecuteProtocolStreamWithAuthManagerAgentUsesSelectionModelForAuth(t *testing.T) {
	selectionModel := "gemini-2.5-flash"
	agentModel := "agents/test-agent"
	requestBody := []byte(`{"agent":"agents/test-agent","input":"hi","stream":true}`)
	executor := &modelExecutionCaptureExecutor{
		provider: constant.GeminiInteractions,
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"id":"interaction_1"}`)}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "model-execution-agent-stream-selection",
		Provider: constant.GeminiInteractions,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "agent-stream-selection@example.com"},
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: selectionModel}, {ID: agentModel}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("manager.Register(): %v", errRegister)
	}
	manager.RefreshSchedulerEntry(auth.ID)
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

	stream, errMsg := handler.ExecuteProtocolStreamWithAuthManager(context.Background(), ProtocolExecutionRequest{
		EntryProtocol:      "interactions",
		ExitProtocol:       "interactions",
		ForcedProvider:     constant.GeminiInteractions,
		AuthSelectionModel: selectionModel,
		Model:              agentModel,
		Stream:             true,
		Body:               requestBody,
	})
	if errMsg != nil {
		t.Fatalf("ExecuteProtocolStreamWithAuthManager() error = %+v", errMsg)
	}
	chunk, ok := <-stream.Chunks
	if !ok {
		t.Fatal("stream chunks closed before payload")
	}
	if chunk.Err != nil {
		t.Fatalf("stream chunk error = %+v", chunk.Err)
	}
	if string(chunk.Payload) != `{"id":"interaction_1"}` {
		t.Fatalf("stream chunk payload = %q, want native interactions response", chunk.Payload)
	}
	gotReq, gotOpts := executor.captured()
	if gotReq.Model != agentModel {
		t.Fatalf("executor model = %q, want %q", gotReq.Model, agentModel)
	}
	if string(gotReq.Payload) != string(requestBody) {
		t.Fatalf("executor payload = %q, want %q", gotReq.Payload, requestBody)
	}
	if gotOpts.Metadata[coreexecutor.AuthSelectionModelMetadataKey] != selectionModel {
		t.Fatalf("auth selection metadata = %#v, want %q", gotOpts.Metadata[coreexecutor.AuthSelectionModelMetadataKey], selectionModel)
	}
}

func TestProvidersForExecutionForcedGeminiRejectsRouterProvider(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	decision := modelRouteDecision{Provider: "claude", Model: "claude-sonnet-4"}
	_, _, errMsg := handler.providersForExecution("agents/test-agent", "agents/test-agent", false, decision, modelExecutionOptions{ForcedProvider: "gemini"})
	if errMsg == nil {
		t.Fatal("providersForExecution() error = nil, want native interactions error")
	}
	if errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusBadRequest)
	}
	if errMsg.Error == nil || !strings.Contains(errMsg.Error.Error(), "native interactions") {
		t.Fatalf("error = %v, want native interactions message", errMsg.Error)
	}
}

func TestProvidersForExecutionForcedGeminiUsesGeminiProvider(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	providers, model, errMsg := handler.providersForExecution("agents/test-agent", "agents/test-agent", false, modelRouteDecision{}, modelExecutionOptions{ForcedProvider: "gemini"})
	if errMsg != nil {
		t.Fatalf("providersForExecution() error = %+v", errMsg)
	}
	if len(providers) != 1 || providers[0] != "gemini" {
		t.Fatalf("providers = %#v, want [gemini]", providers)
	}
	if model != "agents/test-agent" {
		t.Fatalf("model = %q, want agents/test-agent", model)
	}
}
