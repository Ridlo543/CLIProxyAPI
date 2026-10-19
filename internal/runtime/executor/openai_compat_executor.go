package executor

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	openAICompatImageHandlerType            = "openai-image"
	openAICompatImagesGenerationsPath       = "/images/generations"
	openAICompatImagesEditsPath             = "/images/edits"
	openAICompatDefaultImageEndpoint        = openAICompatImagesGenerationsPath
	openAICompatMultipartMemory       int64 = 32 << 20
)

// OpenAICompatExecutor implements a stateless executor for OpenAI-compatible providers.
// It performs request/response translation and executes against the provider base URL
// using per-auth credentials (API key) and per-auth HTTP transport (proxy) from context.
type OpenAICompatExecutor struct {
	provider string
	cfg      *config.Config
}

// NewOpenAICompatExecutor creates an executor bound to a provider key (e.g., "openrouter").
func NewOpenAICompatExecutor(provider string, cfg *config.Config) *OpenAICompatExecutor {
	return &OpenAICompatExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenAICompatExecutor) Identifier() string { return e.provider }

// PrepareRequest injects OpenAI-compatible credentials into the outgoing HTTP request.
func (e *OpenAICompatExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	baseURL, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	applyOpenCodeZenHeaders(req, baseURL, apiKey)
	return nil
}

// HttpRequest injects OpenAI-compatible credentials into the request and executes it.
func (e *OpenAICompatExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("openai compat executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := e.newHTTPClient(ctx, auth, 0)
	return doWithTransientRetry(httpClient, httpReq)
}

func (e *OpenAICompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImages(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to, endpoint := e.upstreamProtocol(auth, req, baseModel, baseURL, opts.Alt)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated, translated, updatesChanged, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, opts.Stream, isCompat)
	if err != nil {
		return resp, err
	}

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged)
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	if helps.ShouldNormalizeOpenAIToolResultsForModel(e.resolveCompatConfig(auth, req), baseModel, requestedModel) {
		translated = helps.NormalizeOpenAIToolResultsTextOnly(translated)
	}
	if opts.Alt != "responses/compact" {
		useMCT := helps.ShouldUseMaxCompletionTokensForModel(e.resolveCompatConfig(auth, req), baseModel, requestedModel)
		translated = helps.NormalizeOpenAIMaxTokens(translated, useMCT)
		translated, err = e.applyPromptCacheKey(ctx, auth, from, baseModel, req, opts, translated)
		if err != nil {
			return resp, err
		}
	}
	if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
		translated = sanitizeOpenAIResponsesReasoningEncryptedContent(ctx, "openai compat executor", translated)
	}
	if endpoint == responsesEndpoint && !opts.Stream {
		if updated, errSet := sjson.SetBytes(translated, "stream", true); errSet == nil {
			translated = updated
		}
	}
	if endpoint == responsesEndpoint {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream_options"); errDelete == nil {
			translated = updated
		}
		translated = ensureZenGateTools(translated)
	}
	if endpoint == chatCompletionsEndpoint && isZenChatUpstream(baseURL) {
		if !opts.Stream {
			if updated, errSet := sjson.SetBytes(translated, "stream", true); errSet == nil {
				translated = updated
			}
		}
		translated = ensureZenChatGateTools(translated)
	}
	if endpoint == chatCompletionsEndpoint {
		// Match on the client-requested model so a persona stays scoped to the
		// one model listed in system-instruction-models. The injector also
		// rewrites the payload model to the upstream name.
		translated = translator.InjectOpenAICompatSystemInstruction(requestedModel, translated, e.resolveCompatConfig(auth, req))
	}
	if endpoint == systemOneEndpoint {
		translated = buildOpenCodeSystemOneRequest(baseModel, translated)
	}
	reporter.SetTranslatedReasoningEffort(translated, to.String())

	url := upstreamURL(baseURL, endpoint)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	applyOpenCodeZenHeaders(httpReq, baseURL, apiKey)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translated,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := e.newHTTPClient(ctx, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := doWithTransientRetry(httpClient, httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newOpenAICompatStatusError(httpResp.StatusCode, httpResp.Header, b)
		return resp, err
	}
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	if endpoint == responsesEndpoint && responsesBodyIsSSE(httpResp.Header.Get("Content-Type"), body) {
		body, err = reassembleResponsesSSE(body)
		if err != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, err)
			return resp, err
		}
	}
	if endpoint == chatCompletionsEndpoint && isZenChatUpstream(baseURL) && !opts.Stream && responsesBodyIsSSE(httpResp.Header.Get("Content-Type"), body) {
		body, err = reassembleChatCompletionsSSE(body)
		if err != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, err)
			return resp, err
		}
	}
	if endpoint == systemOneEndpoint {
		body = wrapSystemOneAsChatCompletion(body, baseModel)
	}
	reporter.ObserveResponseModel(body)
	if endpoint == responsesEndpoint {
		body = completedResponseEnvelope(body)
	}
	// Ensure we at least record the request even if upstream doesn't return usage
	// Translate response back to source format when needed
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), translated, body, &param)
	if helps.ApplyPatchTranslationError(param) != nil || len(out) == 0 {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *OpenAICompatExecutor) executeImages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return resp, err
	}

	payload, contentType, errPrepare := prepareOpenAICompatImagesPayload(req.Payload, baseModel, opts.Headers.Get("Content-Type"), false)
	if errPrepare != nil {
		err = errPrepare
		return resp, err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	payload, contentType, errPrepare = helps.ApplyMediaPayloadConfig(e.cfg, e.Identifier(), baseModel, "openai", payload, contentType, req, opts)
	if errPrepare != nil {
		return resp, errPrepare
	}
	reporter.SetTranslatedReasoningEffort(payload, "openai")

	url := upstreamURL(baseURL, endpointPath)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payload,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := e.newHTTPClient(ctx, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := doWithTransientRetry(httpClient, httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	body, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		err = errRead
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		err = newOpenAICompatStatusError(httpResp.StatusCode, httpResp.Header, body)
		return resp, err
	}

	reporter.ObserveResponseModel(body)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
	reporter.EnsurePublished(ctx)
	resp = cliproxyexecutor.Response{Payload: body, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *OpenAICompatExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImagesStream(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to, endpoint := e.upstreamProtocol(auth, req, baseModel, baseURL, opts.Alt)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated, translated, updatesChanged, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, true, isCompat)
	if err != nil {
		return nil, err
	}

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged)
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	if helps.ShouldNormalizeOpenAIToolResultsForModel(e.resolveCompatConfig(auth, req), baseModel, requestedModel) {
		translated = helps.NormalizeOpenAIToolResultsTextOnly(translated)
	}
	if opts.Alt != "responses/compact" {
		useMCT := helps.ShouldUseMaxCompletionTokensForModel(e.resolveCompatConfig(auth, req), baseModel, requestedModel)
		translated = helps.NormalizeOpenAIMaxTokens(translated, useMCT)
		translated, err = e.applyPromptCacheKey(ctx, auth, from, baseModel, req, opts, translated)
		if err != nil {
			return nil, err
		}
	}

	// Request usage data in the final streaming chunk so that token statistics
	// are captured even when the upstream is an OpenAI-compatible provider.
	if endpoint == chatCompletionsEndpoint {
		translated = helps.SetBoolIfDifferent(translated, "stream_options.include_usage", true)
	}
	if endpoint == responsesEndpoint {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream_options"); errDelete == nil {
			translated = updated
		}
		translated = ensureZenGateTools(translated)
	}
	if endpoint == chatCompletionsEndpoint && isZenChatUpstream(baseURL) {
		translated = ensureZenChatGateTools(translated)
	}
	if endpoint == chatCompletionsEndpoint {
		// Match on the client-requested model so a persona stays scoped to the
		// one model listed in system-instruction-models. The injector also
		// rewrites the payload model to the upstream name.
		translated = translator.InjectOpenAICompatSystemInstruction(requestedModel, translated, e.resolveCompatConfig(auth, req))
	}
	if endpoint == systemOneEndpoint {
		translated = buildOpenCodeSystemOneRequest(baseModel, translated)
	}
	reporter.SetTranslatedReasoningEffort(translated, to.String())

	url := upstreamURL(baseURL, endpoint)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	applyOpenCodeZenHeaders(httpReq, baseURL, apiKey)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translated,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := e.newHTTPClient(ctx, auth, 0)
	// Early role/empty frames are only first-packet fallbacks, not effective tokens.
	httpClient = reporter.TrackHTTPClientRoundTripOnly(httpClient)
	httpResp, err := doWithTransientRetry(httpClient, httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		err = newOpenAICompatStatusError(httpResp.StatusCode, httpResp.Header, b)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
		}()
		var streamReader io.Reader = httpResp.Body
		if endpoint == systemOneEndpoint {
			rawBody, errRead := io.ReadAll(httpResp.Body)
			if errRead == nil {
				streamReader = bytes.NewReader(wrapSystemOneAsChatSSE(rawBody, baseModel))
			}
		}
		scanner := bufio.NewScanner(streamReader)
		scanner.Buffer(nil, 52_428_800) // 50MB
		claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
		var param any
		helps.InitializeApplyPatchStream(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), translated, &param)
		var streamUsage helps.StreamUsageBuffer
		var seenDone bool
		var streamFailed bool
		var streamAborted bool
		var upstreamEvent string
		var frameData [][]byte
		var sawFinishReasonInStream bool
		var sawToolCallsInStream bool
		var lastModelVersion string
		var lastResponseID string
		defer streamUsage.Publish(ctx, reporter)

		publishStreamError := func(streamErr statusErr, containsPayload bool) {
			loggedErr := streamErr
			if containsPayload {
				loggedErr = statusErr{code: streamErr.code, msg: "upstream stream returned an error payload"}
			}
			helps.RecordAPIResponseError(ctx, e.cfg, loggedErr)
			reporter.PublishFailure(ctx, loggedErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
			case <-ctx.Done():
			}
			streamFailed = true
		}

		processFrame := func() bool {
			eventName := upstreamEvent
			upstreamEvent = ""
			dataLines := frameData
			frameData = nil
			if len(dataLines) == 0 {
				if openAICompatErrorEvent(eventName) {
					publishStreamError(statusErr{code: http.StatusBadGateway, msg: "upstream error event ended without data"}, false)
					return true
				}
				return false
			}

			if len(dataLines) > 1 {
				for _, dataLine := range dataLines {
					if bytes.Equal(bytes.TrimSpace(dataLine), []byte("[DONE]")) {
						publishStreamError(statusErr{code: http.StatusBadGateway, msg: "upstream stream ended with incomplete data before [DONE]"}, false)
						return true
					}
				}
			}
			dataPayload := bytes.TrimSpace(bytes.Join(dataLines, []byte("\n")))
			isDone := bytes.Equal(dataPayload, []byte("[DONE]"))
			if isDone && openAICompatErrorEvent(eventName) {
				publishStreamError(statusErr{code: http.StatusBadGateway, msg: "upstream error event ended before [DONE]"}, false)
				return true
			}
			if !isDone && !json.Valid(dataPayload) {
				publishStreamError(statusErr{code: http.StatusBadGateway, msg: "upstream stream ended with incomplete SSE data frame"}, false)
				return true
			}
			if !isDone {
				if streamErr, isError := openAICompatStreamDataError(dataPayload, eventName); isError {
					publishStreamError(streamErr, true)
					return true
				}
			}
			if !isDone {
				if id := gjson.GetBytes(dataPayload, "id").String(); id != "" {
					lastResponseID = id
				}
				if model := gjson.GetBytes(dataPayload, "model").String(); model != "" {
					lastModelVersion = model
				}
				for _, choice := range gjson.GetBytes(dataPayload, "choices").Array() {
					if fr := choice.Get("finish_reason"); fr.Exists() && fr.Type != gjson.Null && fr.String() != "" {
						sawFinishReasonInStream = true
					}
					if tc := choice.Get("delta.tool_calls"); tc.Exists() && tc.IsArray() && len(tc.Array()) > 0 {
						sawToolCallsInStream = true
					}
				}
			}

			// Observe complete upstream frames before translation or downstream backpressure.
			if endpoint == responsesEndpoint {
				helps.ObserveResponsesTokenEvent(reporter, dataPayload)
			} else {
				helps.ObserveChatTokenEvent(reporter, dataPayload)
			}
			if isDone && !sawFinishReasonInStream && (endpoint == "" || endpoint == chatCompletionsEndpoint) {
				finishReason := "stop"
				if sawToolCallsInStream {
					finishReason = "tool_calls"
				}
				modelName := lastModelVersion
				if modelName == "" {
					modelName = baseModel
				}
				respID := lastResponseID
				if respID == "" {
					respID = fmt.Sprintf("chatcmpl_%d", time.Now().UnixNano())
				}
				synthChunk := fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, respID, time.Now().Unix(), modelName, finishReason)
				synthLine := append([]byte("data: "), []byte(synthChunk)...)
				synthChunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), translated, synthLine, &param, claudeInputTokens)
				for i := range synthChunks {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: synthChunks[i]}:
					case <-ctx.Done():
						streamAborted = true
						return true
					}
				}
				sawFinishReasonInStream = true
			}
			streamLine := append([]byte("data: "), dataPayload...)
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), translated, streamLine, &param, claudeInputTokens)
			helps.RecordApplyPatchStreamFailure(ctx, param, reporter, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					streamAborted = true
					return true
				}
			}
			if helps.ApplyPatchTranslationError(param) != nil {
				publishStreamError(statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}, false)
				return true
			}
			if endpoint == responsesEndpoint && !isDone {
				if evType := gjson.GetBytes(dataPayload, "type").String(); evType == "response.completed" || evType == "response.incomplete" {
					seenDone = true
					return true
				}
			}
			if isDone {
				seenDone = true
				return true
			}
			return false
		}

	scanLoop:
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			streamUsage.ObserveOpenAIStream(line)
			trimmedLine := bytes.TrimSpace(line)
			if len(trimmedLine) == 0 {
				if processFrame() {
					break scanLoop
				}
				continue
			}
			if bytes.HasPrefix(trimmedLine, []byte("data:")) {
				frameData = append(frameData, bytes.Clone(bytes.TrimSpace(trimmedLine[len("data:"):])))
				continue
			}
			if bytes.HasPrefix(trimmedLine, []byte("event:")) {
				upstreamEvent = strings.TrimSpace(string(trimmedLine[len("event:"):]))
				continue
			}
			if bytes.HasPrefix(trimmedLine, []byte(":")) || bytes.HasPrefix(trimmedLine, []byte("id:")) || bytes.HasPrefix(trimmedLine, []byte("retry:")) {
				continue
			}
			if bytes.HasPrefix(trimmedLine, []byte("{")) || bytes.HasPrefix(trimmedLine, []byte("[")) {
				publishStreamError(statusErr{code: http.StatusBadGateway, msg: string(trimmedLine)}, true)
				break
			}
		}
		errScan := scanner.Err()
		if errScan == nil && !seenDone && !streamFailed && !streamAborted && len(frameData) > 0 {
			_ = processFrame()
		}
		if !streamFailed && !streamAborted && helps.EndApplyPatchStream(ctx, param, reporter, out, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}) {
			return
		}
		if !streamFailed && !streamAborted && !seenDone && errScan == nil && responseFormat == sdktranslator.FormatOpenAIResponse && ctx.Err() == nil && helps.CanFinalizeResponseStream(param) {
			finalChunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), translated, []byte("data: [DONE]"), &param, claudeInputTokens)
			for _, finalChunk := range finalChunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: finalChunk}:
				case <-ctx.Done():
					streamAborted = true
				}
				if streamAborted {
					break
				}
			}
			if len(finalChunks) > 0 && !streamAborted {
				seenDone = true
			}
		}
		if streamFailed || streamAborted {
			return
		}
		if errScan != nil && !seenDone {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		} else if !seenDone {
			// Without a translator-confirmed terminal state, a clean Responses EOF
			// without [DONE] remains a failed stream instead of completing it.
			if responseFormat == sdktranslator.FormatOpenAIResponse {
				streamErr := statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before [DONE]"}
				helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
				reporter.PublishFailure(ctx, streamErr)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
				case <-ctx.Done():
				}
				return
			}

			if !sawFinishReasonInStream && (endpoint == "" || endpoint == chatCompletionsEndpoint) {
				finishReason := "stop"
				if sawToolCallsInStream {
					finishReason = "tool_calls"
				}
				modelName := lastModelVersion
				if modelName == "" {
					modelName = baseModel
				}
				respID := lastResponseID
				if respID == "" {
					respID = fmt.Sprintf("chatcmpl_%d", time.Now().UnixNano())
				}
				synthChunk := fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, respID, time.Now().Unix(), modelName, finishReason)
				synthLine := append([]byte("data: "), []byte(synthChunk)...)
				synthChunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), translated, synthLine, &param, claudeInputTokens)
				for i := range synthChunks {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: synthChunks[i]}:
					case <-ctx.Done():
						return
					}
				}
				sawFinishReasonInStream = true
			}
			// Other protocols retain compatibility with providers that omit [DONE].
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), translated, []byte("data: [DONE]"), &param, claudeInputTokens)
			helps.RecordApplyPatchStreamFailure(ctx, param, reporter, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		// Ensure we record the request if no usage chunk was ever seen.
		streamUsage.Publish(ctx, reporter)
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OpenAICompatExecutor) executeImagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	payload, contentType, errPrepare := prepareOpenAICompatImagesPayload(req.Payload, baseModel, opts.Headers.Get("Content-Type"), true)
	if errPrepare != nil {
		err = errPrepare
		return nil, err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	payload, contentType, errPrepare = helps.ApplyMediaPayloadConfig(e.cfg, e.Identifier(), baseModel, "openai", payload, contentType, req, opts)
	if errPrepare != nil {
		return nil, errPrepare
	}
	reporter.SetTranslatedReasoningEffort(payload, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payload,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := e.newHTTPClient(ctx, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := doWithTransientRetry(httpClient, httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, body)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		return nil, statusErr{code: httpResp.StatusCode, msg: string(body)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		observer := helps.NewStreamResponseModelObserver(reporter)
		defer func() {
			observer.Finish()
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
			reporter.EnsurePublished(ctx)
		}()
		buffer := make([]byte, 32*1024)
		for {
			n, errRead := httpResp.Body.Read(buffer)
			if n > 0 {
				chunk := bytes.Clone(buffer[:n])
				helps.AppendAPIResponseChunk(ctx, e.cfg, chunk)
				observer.Feed(chunk)
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-ctx.Done():
					return
				}
			}
			if errRead != nil {
				if errRead != io.EOF {
					helps.RecordAPIResponseError(ctx, e.cfg, errRead)
					reporter.PublishFailure(ctx, errRead)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OpenAICompatExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("openai")
	isCompat := helps.APIKeyModelIsCompat(req)
	translated, updatesChanged, err := helps.TranslateRequestWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, false, isCompat)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	originalTranslatedForPayload := append([]byte(nil), translated...)
	if len(opts.OriginalRequest) > 0 {
		originalTranslatedForPayload, _, _ = helps.TranslateRequestWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, baseModel, opts.OriginalRequest, false, isCompat)
	}

	modelForCounting := baseModel

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	translated = helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "", originalTranslatedForPayload, req, opts)(translated)
	enc, err := helps.TokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

// Refresh is a no-op for API-key based compatibility providers.
// OAuth-style credentials with a refresh token cannot be rotated here; callers
// that need plugin/Home refresh must bind a refresh-capable executor instead.
func (e *OpenAICompatExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("openai compat executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if openAICompatAuthHasRefreshToken(auth) {
		provider := ""
		if e != nil {
			provider = e.Identifier()
		}
		if provider == "" && auth != nil {
			provider = strings.TrimSpace(auth.Provider)
		}
		return nil, fmt.Errorf("openai compat executor cannot refresh oauth credentials for provider %s", provider)
	}
	return auth, nil
}

func openAICompatAuthHasRefreshToken(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	if token, _ := auth.Metadata["refresh_token"].(string); strings.TrimSpace(token) != "" {
		return true
	}
	if token, _ := auth.Metadata["refreshToken"].(string); strings.TrimSpace(token) != "" {
		return true
	}
	return false
}

func openAICompatImageEndpointPath(opts cliproxyexecutor.Options) string {
	if opts.SourceFormat.String() != openAICompatImageHandlerType {
		return ""
	}
	path := helps.PayloadRequestPath(opts)
	if strings.HasSuffix(path, "/images/edits") {
		return openAICompatImagesEditsPath
	}
	if strings.HasSuffix(path, "/images/generations") {
		return openAICompatImagesGenerationsPath
	}
	return openAICompatDefaultImageEndpoint
}

func prepareOpenAICompatImagesPayload(payload []byte, model string, contentType string, stream bool) ([]byte, string, error) {
	model = strings.TrimSpace(model)
	contentType = strings.TrimSpace(contentType)
	if json.Valid(payload) {
		if model != "" {
			payload = helps.SetStringIfDifferent(payload, "model", model)
		}
		if stream {
			payload = helps.SetBoolIfDifferent(payload, "stream", true)
		} else {
			payload, _ = sjson.DeleteBytes(payload, "stream")
		}
		return payload, "application/json", nil
	}

	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "multipart/") {
		return payload, contentType, nil
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is missing")
	}
	return rewriteOpenAICompatImagesMultipartPayload(payload, model, boundary, stream)
}

func cloneOpenAICompatMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func rewriteOpenAICompatImagesMultipartPayload(payload []byte, model string, boundary string, stream bool) ([]byte, string, error) {
	reader := multipart.NewReader(bytes.NewReader(payload), boundary)
	form, errRead := reader.ReadForm(openAICompatMultipartMemory)
	if errRead != nil {
		return nil, "", fmt.Errorf("read multipart form failed: %w", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			log.Errorf("openai compat executor: remove multipart form files error: %v", errRemove)
		}
	}()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if model != "" {
		if errWrite := writer.WriteField("model", model); errWrite != nil {
			return nil, "", fmt.Errorf("write model field failed: %w", errWrite)
		}
	}
	if stream {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return nil, "", fmt.Errorf("write stream field failed: %w", errWrite)
		}
	}
	for key, values := range form.Value {
		if key == "model" || key == "stream" {
			continue
		}
		for _, value := range values {
			if errWrite := writer.WriteField(key, value); errWrite != nil {
				return nil, "", fmt.Errorf("write form field %s failed: %w", key, errWrite)
			}
		}
	}
	for key, files := range form.File {
		for _, fileHeader := range files {
			if fileHeader == nil {
				continue
			}
			header := cloneOpenAICompatMIMEHeader(fileHeader.Header)
			header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "application/octet-stream")
			}
			part, errCreate := writer.CreatePart(header)
			if errCreate != nil {
				return nil, "", fmt.Errorf("create file field %s failed: %w", key, errCreate)
			}
			src, errOpen := fileHeader.Open()
			if errOpen != nil {
				return nil, "", fmt.Errorf("open upload file failed: %w", errOpen)
			}
			_, errCopy := io.Copy(part, src)
			if errClose := src.Close(); errClose != nil {
				log.Errorf("openai compat executor: close upload file error: %v", errClose)
				if errCopy == nil {
					errCopy = errClose
				}
			}
			if errCopy != nil {
				return nil, "", fmt.Errorf("copy upload file failed: %w", errCopy)
			}
		}
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func (e *OpenAICompatExecutor) applyPromptCacheKey(ctx context.Context, auth *cliproxyauth.Auth, from sdktranslator.Format, baseModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, translated []byte) ([]byte, error) {
	compat := e.resolveCompatConfig(auth, req)
	if compat == nil || !compat.SupportPromptCacheKey {
		return translated, nil
	}

	for _, payload := range [][]byte{req.Payload, opts.OriginalRequest, translated} {
		if promptCacheKey := strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_key").String()); promptCacheKey != "" {
			return helps.SetStringIfDifferent(translated, "prompt_cache_key", promptCacheKey), nil
		}
	}

	modelName := strings.TrimSpace(gjson.GetBytes(translated, "model").String())
	if modelName == "" {
		modelName = baseModel
	}
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		cached, ok, errCache := helps.ClaudeCodePromptCache(ctx, modelName, req.Payload, opts.Headers)
		if errCache != nil {
			return translated, errCache
		}
		if ok {
			return helps.SetStringIfDifferent(translated, "prompt_cache_key", cached.ID), nil
		}
	}

	sessionID := helps.ProviderSessionUUID(e.provider, opts.Metadata, req.Metadata)
	if sessionID == "" {
		return translated, nil
	}
	provider := strings.TrimSpace(e.provider)
	if provider == "" {
		provider = strings.TrimSpace(compat.Name)
	}
	identity := strings.Join([]string{
		"cli-proxy-api:openai-compat:prompt-cache",
		strings.ToLower(provider),
		strings.ToLower(modelName),
		strings.ToLower(strings.TrimSpace(from.String())),
		sessionID,
	}, "\x00")
	promptCacheKey := uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity)).String()
	return helps.SetStringIfDifferent(translated, "prompt_cache_key", promptCacheKey), nil
}

func (e *OpenAICompatExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

func (e *OpenAICompatExecutor) resolveCompatConfig(auth *cliproxyauth.Auth, req cliproxyexecutor.Request) *config.OpenAICompatibility {
	if auth == nil || e.cfg == nil {
		return nil
	}
	if e.cfg.Home.Enabled {
		// Home excludes provider credentials from the global configuration. These
		// non-secret options belong to the credential selected for this attempt.
		var options struct {
			SupportPromptCacheKey bool                              `json:"support-prompt-cache-key"`
			Models                []config.OpenAICompatibilityModel `json:"models"`
		}
		present := false
		if rawOptions, exists := auth.Metadata["credential_options"]; exists {
			if data, errMarshal := json.Marshal(rawOptions); errMarshal == nil {
				present = json.Unmarshal(data, &options) == nil
			}
		}
		if raw, exists := auth.Attributes["support_prompt_cache_key"]; exists {
			if value, errParse := strconv.ParseBool(raw); errParse == nil {
				options.SupportPromptCacheKey = value
				present = true
			}
		}
		if model, ok := cliproxyauth.ResolvedHomeModelOptions(req); ok {
			options.Models = []config.OpenAICompatibilityModel{model}
			present = true
		}
		if present {
			return &config.OpenAICompatibility{
				Name:                  auth.Attributes["compat_name"],
				SupportPromptCacheKey: options.SupportPromptCacheKey,
				Models:                options.Models,
			}
		}
	}
	if auth.AuthSourceKind() == cliproxyauth.AuthSourceConfig && auth.Attributes != nil {
		if rawIndex := strings.TrimSpace(auth.Attributes["config_index"]); rawIndex != "" {
			configIndex, errIndex := strconv.Atoi(rawIndex)
			if errIndex == nil && configIndex >= 0 && configIndex < len(e.cfg.OpenAICompatibility) {
				compat := &e.cfg.OpenAICompatibility[configIndex]
				if !compat.Disabled {
					return compat
				}
			}
		}
	}
	candidates := make([]string, 0, 3)
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["compat_name"]); v != "" {
			candidates = append(candidates, v)
		}
		if v := strings.TrimSpace(auth.Attributes["provider_key"]); v != "" {
			candidates = append(candidates, v)
		}
	}
	if v := strings.TrimSpace(auth.Provider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range e.cfg.OpenAICompatibility {
		compat := &e.cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func (e *OpenAICompatExecutor) overrideModel(payload []byte, model string) []byte {
	if len(payload) == 0 || model == "" {
		return payload
	}
	return helps.SetStringIfDifferent(payload, "model", model)
}

func openAICompatErrorEvent(eventName string) bool {
	return strings.EqualFold(eventName, "error") || strings.EqualFold(eventName, "response.error") || strings.EqualFold(eventName, "response.failed")
}

func openAICompatStreamDataError(payload []byte, eventName string) (statusErr, bool) {
	if len(payload) == 0 || !json.Valid(payload) {
		return statusErr{}, false
	}
	payloadType := gjson.GetBytes(payload, "type").String()
	hasError := false
	for _, path := range []string{"error", "response.error"} {
		errorNode := gjson.GetBytes(payload, path)
		if errorNode.Exists() && errorNode.Raw != "null" {
			hasError = true
			break
		}
	}
	hasTopLevelErrorFields := gjson.GetBytes(payload, "code").Exists() && gjson.GetBytes(payload, "message").Exists()
	if !hasError && !strings.EqualFold(payloadType, "error") && !strings.EqualFold(payloadType, "response.error") && !strings.EqualFold(payloadType, "response.failed") &&
		!openAICompatErrorEvent(eventName) && !hasTopLevelErrorFields {
		return statusErr{}, false
	}

	status := 0
	for _, path := range []string{"status", "status_code", "error.status", "error.status_code", "response.error.status", "response.error.status_code"} {
		status = int(gjson.GetBytes(payload, path).Int())
		if status >= http.StatusBadRequest && status <= 599 {
			break
		}
	}
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}
	return statusErr{code: status, msg: string(payload)}, true
}

type statusErr struct {
	code             int
	msg              string
	retryAfter       *time.Duration
	credentialScoped bool
	// upstreamRateLimit marks a 429 the provider sent with no quota watermark,
	// so the conductor cools the model without recording exhausted quota.
	upstreamRateLimit bool
}

func (e statusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int            { return e.code }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }
func (e statusErr) IsCredentialScoped() bool   { return e.credentialScoped }
func (e statusErr) UpstreamRateLimit() bool    { return e.upstreamRateLimit }

const openAICompatTPMFallbackRetryAfter = time.Minute

func newOpenAICompatStatusError(status int, headers http.Header, body []byte) statusErr {
	return statusErr{
		code:       status,
		msg:        string(body),
		retryAfter: openAICompatRetryAfter(status, headers, body, time.Now()),
	}
}

func isTransientTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr != nil && netErr.Timeout() {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr != nil {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "unexpected eof")
}

type idleConnectionCloser interface {
	CloseIdleConnections()
}

func doWithTransientRetry(httpClient *http.Client, httpReq *http.Request) (*http.Response, error) {
	httpResp, err := httpClient.Do(httpReq)
	if err != nil && isTransientTransportError(err) && httpReq.GetBody != nil && httpReq.Context().Err() == nil {
		if ic, ok := httpClient.Transport.(idleConnectionCloser); ok && ic != nil {
			ic.CloseIdleConnections()
		}
		if dt, ok := http.DefaultTransport.(idleConnectionCloser); ok && dt != nil {
			dt.CloseIdleConnections()
		}
		if freshBody, bErr := httpReq.GetBody(); bErr == nil {
			httpReq.Body = freshBody
			return httpClient.Do(httpReq)
		}
	}
	return httpResp, err
}

type unwrappableTransport interface {
	Unwrap() http.RoundTripper
	SetBase(http.RoundTripper)
}

func enforceHTTP11(rt http.RoundTripper) http.RoundTripper {
	if u, ok := rt.(unwrappableTransport); ok && u != nil {
		u.SetBase(enforceHTTP11(u.Unwrap()))
		return rt
	}
	if rt == nil {
		if dt, ok := http.DefaultTransport.(*http.Transport); ok && dt != nil {
			rt = dt
		} else {
			return &http.Transport{
				ForceAttemptHTTP2: false,
				TLSNextProto:      make(map[string]func(string, *tls.Conn) http.RoundTripper),
			}
		}
	}
	if tr, ok := rt.(*http.Transport); ok && tr != nil {
		clone := tr.Clone()
		clone.ForceAttemptHTTP2 = false
		clone.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		if clone.TLSClientConfig != nil {
			clone.TLSClientConfig = clone.TLSClientConfig.Clone()
			clone.TLSClientConfig.NextProtos = nil
		}
		return clone
	}
	return rt
}

func (e *OpenAICompatExecutor) newHTTPClient(ctx context.Context, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, timeout)
	httpClient.Transport = enforceHTTP11(httpClient.Transport)
	return httpClient
}

// openAICompatRetryAfter preserves the provider's standard Retry-After signal.
// Some OpenAI-compatible providers omit that header for explicit per-minute
// token limits; in that narrow case a one-minute fallback prevents immediate
// replay of the same large request while keeping the retry wait bounded.
func openAICompatRetryAfter(status int, headers http.Header, body []byte, now time.Time) *time.Duration {
	if status != http.StatusTooManyRequests {
		return nil
	}
	if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
		if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds >= 0 {
			delay := time.Duration(seconds) * time.Second
			return &delay
		}
		if deadline, errParse := http.ParseTime(raw); errParse == nil {
			delay := deadline.Sub(now)
			if delay < 0 {
				delay = 0
			}
			return &delay
		}
	}

	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.message").String()))
	if strings.Contains(code, "tpmratelimitexceeded") ||
		(strings.Contains(message, "tokens per minute") && strings.Contains(message, "limit") && strings.Contains(message, "exceeded")) {
		delay := openAICompatTPMFallbackRetryAfter
		return &delay
	}
	return nil
}

// SupportsApplyPatch reports the actual executor contract, independent of its provider name.
func (e *OpenAICompatExecutor) SupportsApplyPatch() bool { return e != nil }

const (
	// chatCompletionsEndpoint is the default upstream path for compat providers.
	chatCompletionsEndpoint = "/chat/completions"
	// responsesEndpoint is the Responses API path, used by providers that
	// declare `protocol: responses` or serve Responses-only models on OpenCode Zen.
	responsesEndpoint = "/responses"
	// responsesCompactEndpoint keeps the dedicated compact endpoint.
	responsesCompactEndpoint = "/responses/compact"
	// systemOneEndpoint is the TypeSafe SystemOne decision endpoint on OpenCode Zen.
	systemOneEndpoint = "/systemone"
	// responsesProtocol is the per-provider config value selecting the
	// Responses API upstream.
	responsesProtocol = "responses"
	// openCodeZenUserAgent mimics an official OpenCode CLI release (>= 1.17.0)
	// required by the OpenCode Zen free-tier gate.
	openCodeZenUserAgent = "opencode/1.18.35 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
	openCodeBase62Chars  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

var (
	openCodeIDMu        sync.Mutex
	openCodeIDLastMilli int64
	openCodeIDCounter   uint64
)

// generateOpenCodeZenID produces a timestamp-encoded base62 identifier matching
// OpenCode CLI's Identifier.create("ses"|"msg", descending, timestamp) format.
func generateOpenCodeZenID(prefix string, descending bool, timestampMs int64) string {
	openCodeIDMu.Lock()
	if timestampMs != openCodeIDLastMilli {
		openCodeIDLastMilli = timestampMs
		openCodeIDCounter = 0
	}
	openCodeIDCounter++
	counter := openCodeIDCounter
	openCodeIDMu.Unlock()

	current := uint64(timestampMs)*0x1000 + counter
	value := current
	if descending {
		value = ^current
	}
	timeHex := fmt.Sprintf("%012x", value&0xffffffffffff)

	var rawBytes [14]byte
	_, _ = cryptorand.Read(rawBytes[:])
	var suffix [14]byte
	for i, b := range rawBytes {
		suffix[i] = openCodeBase62Chars[int(b)%len(openCodeBase62Chars)]
	}
	return prefix + "_" + timeHex + string(suffix[:])
}

func isOpenCodeZenUpstream(baseURL string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(baseURL))
	return strings.Contains(trimmed, "opencode.ai/zen") ||
		strings.Contains(trimmed, "opencode.ai") ||
		strings.Contains(trimmed, "/zen/")
}

func isOpenCodeZenResponsesModel(baseModel string) bool {
	lower := strings.ToLower(strings.TrimSpace(baseModel))
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		lower = lower[idx+1:]
	}
	return strings.HasPrefix(lower, "muse-spark") ||
		strings.HasPrefix(lower, "grok-4.6") ||
		strings.HasPrefix(lower, "grok-4.7") ||
		strings.HasPrefix(lower, "gpt-5") ||
		strings.HasPrefix(lower, "gpt-6")
}

func isOpenCodeZenSystemOneModel(baseModel string) bool {
	lower := strings.ToLower(strings.TrimSpace(baseModel))
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		lower = lower[idx+1:]
	}
	return strings.HasPrefix(lower, "jev-") || lower == "jev"
}

func applyOpenCodeZenHeaders(httpReq *http.Request, baseURL, apiKey string) {
	if httpReq == nil || !isOpenCodeZenUpstream(baseURL) {
		return
	}
	if strings.TrimSpace(apiKey) == "" && strings.TrimSpace(httpReq.Header.Get("Authorization")) == "" {
		httpReq.Header.Set("Authorization", "Bearer public")
	}
	ua := strings.TrimSpace(httpReq.Header.Get("User-Agent"))
	if ua == "" || ua == "cli-proxy-openai-compat" || !strings.HasPrefix(strings.ToLower(ua), "opencode/") {
		httpReq.Header.Set("User-Agent", openCodeZenUserAgent)
	}
	if strings.TrimSpace(httpReq.Header.Get("x-opencode-client")) == "" {
		httpReq.Header.Set("x-opencode-client", "cli")
	}
	if strings.TrimSpace(httpReq.Header.Get("x-opencode-project")) == "" {
		httpReq.Header.Set("x-opencode-project", "global")
	}
	nowMs := time.Now().UnixMilli()
	if sess := strings.TrimSpace(httpReq.Header.Get("x-opencode-session")); !strings.HasPrefix(sess, "ses_") || len(sess) < 20 {
		httpReq.Header.Set("x-opencode-session", generateOpenCodeZenID("ses", true, nowMs-3000))
	}
	if reqID := strings.TrimSpace(httpReq.Header.Get("x-opencode-request")); !strings.HasPrefix(reqID, "msg_") || len(reqID) < 20 {
		httpReq.Header.Set("x-opencode-request", generateOpenCodeZenID("msg", false, nowMs))
	}
}

// upstreamProtocol returns the translator format and the endpoint path for this
// provider and model.
func (e *OpenAICompatExecutor) upstreamProtocol(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, baseModel, baseURL, alt string) (sdktranslator.Format, string) {
	switch {
	case alt == "responses/compact":
		return sdktranslator.FromString("openai-response"), responsesCompactEndpoint
	case isOpenCodeZenUpstream(baseURL) && isOpenCodeZenSystemOneModel(baseModel):
		return sdktranslator.FromString("openai"), systemOneEndpoint
	case providerUsesResponsesProtocol(e.resolveCompatConfig(auth, req), baseModel, baseURL):
		return sdktranslator.FromString("codex"), responsesEndpoint
	default:
		return sdktranslator.FromString("openai"), chatCompletionsEndpoint
	}
}

func providerUsesResponsesProtocol(compat *config.OpenAICompatibility, baseModel, baseURL string) bool {
	if compat != nil {
		if strings.EqualFold(strings.TrimSpace(compat.Protocol), responsesProtocol) {
			return true
		}
		if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(compat.BaseURL), "/")), responsesEndpoint) {
			return true
		}
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(baseURL), "/")), responsesEndpoint) {
		return true
	}
	return isOpenCodeZenUpstream(baseURL) && isOpenCodeZenResponsesModel(baseModel)
}

func isZenChatUpstream(baseURL string) bool {
	trimmed := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(baseURL), "/"))
	if !strings.Contains(trimmed, "opencode.ai/zen") {
		return false
	}
	return !strings.HasSuffix(trimmed, responsesEndpoint) && !strings.HasSuffix(trimmed, responsesCompactEndpoint)
}

var zenChatGateToolDefs = []struct{ name, def string }{
	{"bash", `{"type":"function","function":{"name":"bash","description":"Executes a bash command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}`},
	{"read", `{"type":"function","function":{"name":"read","description":"Reads a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}`},
}

func ensureZenChatGateTools(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	seen := map[string]bool{}
	if tools.IsArray() {
		for _, item := range tools.Array() {
			if name := item.Get("name").String(); name != "" {
				seen[name] = true
				continue
			}
			if name := item.Get("function.name").String(); name != "" {
				seen[name] = true
			}
		}
	}
	for _, tool := range zenChatGateToolDefs {
		if seen[tool.name] {
			continue
		}
		var updated []byte
		var err error
		if tools.IsArray() {
			updated, err = sjson.SetRawBytes(payload, "tools."+strconv.Itoa(len(tools.Array())), []byte(tool.def))
		} else {
			updated, err = sjson.SetRawBytes(payload, "tools", []byte("["+tool.def+"]"))
		}
		if err == nil {
			payload = updated
			tools = gjson.GetBytes(payload, "tools")
		}
	}
	return payload
}

func reassembleChatCompletionsSSE(body []byte) ([]byte, error) {
	var contentSB, reasoningSB strings.Builder
	var id, model, finish string
	var created int64
	var usageRaw string
	toolCallsMap := make(map[int]map[string]any)
	toolCallArgs := make(map[int]*strings.Builder)
	saw := false
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !json.Valid(data) {
			continue
		}
		saw = true
		if v := gjson.GetBytes(data, "id").String(); v != "" {
			id = v
		}
		if v := gjson.GetBytes(data, "model").String(); v != "" {
			model = v
		}
		if v := gjson.GetBytes(data, "created").Int(); v != 0 {
			created = v
		}
		if u := gjson.GetBytes(data, "usage"); u.Exists() && u.Type == gjson.JSON {
			usageRaw = u.Raw
		}
		for _, ch := range gjson.GetBytes(data, "choices").Array() {
			contentSB.WriteString(ch.Get("delta.content").String())
			if r := ch.Get("delta.reasoning").String(); r != "" {
				reasoningSB.WriteString(r)
			} else if rc := ch.Get("delta.reasoning_content").String(); rc != "" {
				reasoningSB.WriteString(rc)
			}
			if v := ch.Get("finish_reason").String(); v != "" && v != "null" {
				finish = v
			}
			for _, tc := range ch.Get("delta.tool_calls").Array() {
				idx := int(tc.Get("index").Int())
				entry, exists := toolCallsMap[idx]
				if !exists {
					entry = map[string]any{
						"index": idx,
						"type":  "function",
						"function": map[string]any{
							"name":      "",
							"arguments": "",
						},
					}
					toolCallsMap[idx] = entry
					toolCallArgs[idx] = &strings.Builder{}
				}
				if tid := tc.Get("id").String(); tid != "" {
					entry["id"] = tid
				}
				if ttype := tc.Get("type").String(); ttype != "" {
					entry["type"] = ttype
				}
				fn := entry["function"].(map[string]any)
				if fname := tc.Get("function.name").String(); fname != "" {
					fn["name"] = fname
				}
				if fargs := tc.Get("function.arguments").String(); fargs != "" {
					toolCallArgs[idx].WriteString(fargs)
				}
			}
		}
	}
	if !saw {
		return nil, statusErr{code: http.StatusBadGateway, msg: "upstream chat stream ended without data events"}
	}
	if finish == "" {
		finish = "stop"
	}
	msg := map[string]any{
		"role":    "assistant",
		"content": contentSB.String(),
	}
	if reasoningSB.Len() > 0 {
		msg["reasoning_content"] = reasoningSB.String()
	}
	if len(toolCallsMap) > 0 {
		var toolCallsList []any
		for i := 0; i < len(toolCallsMap); i++ {
			if tc, ok := toolCallsMap[i]; ok {
				fn := tc["function"].(map[string]any)
				if sb, hasArgs := toolCallArgs[i]; hasArgs {
					fn["arguments"] = sb.String()
				}
				toolCallsList = append(toolCallsList, tc)
			}
		}
		if len(toolCallsList) > 0 {
			msg["tool_calls"] = toolCallsList
			if contentSB.Len() == 0 {
				// If no text content was produced, extract any command/code from bash tool call into content
				for _, item := range toolCallsList {
					if m, ok := item.(map[string]any); ok {
						fn, _ := m["function"].(map[string]any)
						if fn["name"] == "bash" {
							argsStr, _ := fn["arguments"].(string)
							var argsObj struct {
								Command string `json:"command"`
							}
							if json.Unmarshal([]byte(argsStr), &argsObj) == nil && argsObj.Command != "" {
								msg["content"] = argsObj.Command
								break
							}
						}
					}
				}
			}
		}
	}
	root := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	if usageRaw != "" {
		if updated, errSet := sjson.SetRawBytes(out, "usage", []byte(usageRaw)); errSet == nil {
			out = updated
		}
	}
	return out, nil
}

var zenGateToolDefs = []struct{ name, def string }{
	{"bash", `{"type":"function","name":"bash","description":"Executes a bash command","parameters":{"type":"object","properties":{},"required":["command"]}}`},
	{"read", `{"type":"function","name":"read","description":"Reads a file","parameters":{"type":"object","properties":{},"required":["path"]}}`},
}

func ensureZenGateTools(payload []byte) []byte {
	for _, tool := range zenGateToolDefs {
		found := false
		for _, item := range gjson.GetBytes(payload, "tools").Array() {
			if item.Get("name").String() == tool.name {
				found = true
				break
			}
		}
		if found {
			continue
		}
		var updated []byte
		var err error
		if gjson.GetBytes(payload, "tools").IsArray() {
			idx := len(gjson.GetBytes(payload, "tools").Array())
			updated, err = sjson.SetRawBytes(payload, "tools."+strconv.Itoa(idx), []byte(tool.def))
		} else {
			updated, err = sjson.SetRawBytes(payload, "tools", []byte("["+tool.def+"]"))
		}
		if err == nil {
			payload = updated
		}
	}
	return payload
}

func upstreamURL(baseURL, endpoint string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if endpoint == "" {
		return trimmed
	}
	lower := strings.ToLower(trimmed)
	endpointLower := strings.ToLower(endpoint)
	switch {
	case strings.HasSuffix(lower, endpointLower):
		return trimmed
	case strings.HasSuffix(lower, responsesEndpoint) && strings.HasPrefix(endpointLower, responsesEndpoint):
		return trimmed + strings.TrimPrefix(endpoint, responsesEndpoint)
	default:
		return trimmed + endpoint
	}
}

func responsesBodyIsSSE(contentType string, body []byte) bool {
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		return bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:"))
	}
	return false
}

func reassembleResponsesSSE(body []byte) ([]byte, error) {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || !json.Valid(data) {
			continue
		}
		if gjson.GetBytes(data, "type").String() == "response.completed" {
			if raw := gjson.GetBytes(data, "response").Raw; raw != "" {
				return []byte(raw), nil
			}
		}
	}
	return nil, statusErr{code: http.StatusBadGateway, msg: "upstream responses stream ended without a response.completed event"}
}

func completedResponseEnvelope(body []byte) []byte {
	var root map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return body
	}
	if _, hasType := root["type"]; hasType {
		return body
	}
	var meta struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &meta)
	eventType := "response.completed"
	if strings.EqualFold(strings.TrimSpace(meta.Status), "incomplete") {
		eventType = "response.incomplete"
	}
	eventTypeJSON, errMarshal := json.Marshal(eventType)
	if errMarshal != nil {
		return body
	}
	envelope, errMarshal := json.Marshal(map[string]json.RawMessage{
		"type":     eventTypeJSON,
		"response": json.RawMessage(body),
	})
	if errMarshal != nil {
		return body
	}
	return envelope
}

func buildOpenCodeSystemOneRequest(baseModel string, payload []byte) []byte {
	modelName := baseModel
	if idx := strings.LastIndex(modelName, "/"); idx >= 0 {
		modelName = modelName[idx+1:]
	}

	if gjson.GetBytes(payload, "state").Exists() && gjson.GetBytes(payload, "questions").Exists() {
		// OpenCode Zen free /systemone only natively accepts type: "noul".
		// Normalize any "choice", "boolean", or other question types into noul instructions
		// so upstream accepts the request cleanly.
		transformedQuestions := map[string]any{}
		for k, q := range gjson.GetBytes(payload, "questions").Map() {
			qType := q.Get("type").String()
			prompt := q.Get("prompt").String()
			if prompt == "" {
				prompt = q.Get("instructions").String()
			}
			if qType == "choice" {
				var optTexts []string
				for i, opt := range q.Get("options").Array() {
					optTexts = append(optTexts, fmt.Sprintf("(%d) %s", i+1, opt.String()))
				}
				transformedQuestions[k] = map[string]any{
					"type":         "noul",
					"instructions": fmt.Sprintf("Choice question: %s Options: %s. Evaluate the most sound option.", prompt, strings.Join(optTexts, "; ")),
				}
			} else if qType == "boolean" {
				transformedQuestions[k] = map[string]any{
					"type":         "noul",
					"instructions": fmt.Sprintf("Is this true or affirmative? %s", prompt),
				}
			} else {
				transformedQuestions[k] = map[string]any{
					"type":         "noul",
					"instructions": prompt,
				}
			}
		}

		reqObj := map[string]any{
			"model":     modelName,
			"state":     gjson.GetBytes(payload, "state").String(),
			"questions": transformedQuestions,
		}
		if out, err := json.Marshal(reqObj); err == nil {
			return out
		}
		return payload
	}

	var stateParts []string
	lastUser := "Is this input valid and actionable?"
	for _, msg := range gjson.GetBytes(payload, "messages").Array() {
		role := msg.Get("role").String()
		content := msg.Get("content")
		text := ""
		if content.IsArray() {
			var parts []string
			for _, p := range content.Array() {
				if t := p.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
			}
			text = strings.Join(parts, "\n")
		} else {
			text = content.String()
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		stateParts = append(stateParts, role+": "+text)
		if role == "user" {
			lastUser = text
		}
	}
	stateText := strings.Join(stateParts, "\n\n")
	if stateText == "" {
		stateText = lastUser
	}
	reqObj := map[string]any{
		"model": modelName,
		"state": stateText,
		"questions": map[string]any{
			"verdict": map[string]any{
				"type":         "noul",
				"instructions": lastUser,
			},
		},
	}
	if out, err := json.Marshal(reqObj); err == nil {
		return out
	}
	return payload
}

func wrapSystemOneAsChatCompletion(body []byte, baseModel string) []byte {
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		model = baseModel
	}
	content := string(bytes.TrimSpace(body))
	if answers := gjson.GetBytes(body, "answers"); answers.Exists() {
		content = answers.Raw
	}
	inTok := gjson.GetBytes(body, "usage.input_tokens").Int()
	outTok := gjson.GetBytes(body, "usage.output_tokens").Int()
	respObj := map[string]any{
		"id":      "jev-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     inTok,
			"completion_tokens": outTok,
			"total_tokens":      inTok + outTok,
		},
	}
	out, err := json.Marshal(respObj)
	if err != nil {
		return body
	}
	return out
}

func wrapSystemOneAsChatSSE(body []byte, baseModel string) []byte {
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		model = baseModel
	}
	content := string(bytes.TrimSpace(body))
	if answers := gjson.GetBytes(body, "answers"); answers.Exists() {
		content = answers.Raw
	}
	inTok := gjson.GetBytes(body, "usage.input_tokens").Int()
	outTok := gjson.GetBytes(body, "usage.output_tokens").Int()
	chunkObj := map[string]any{
		"id":      "jev-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"finish_reason": "stop",
				"delta": map[string]any{
					"role":    "assistant",
					"content": content,
				},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     inTok,
			"completion_tokens": outTok,
			"total_tokens":      inTok + outTok,
		},
	}
	chunkBytes, _ := json.Marshal(chunkObj)
	return []byte("data: " + string(chunkBytes) + "\n\ndata: [DONE]\n\n")
}
