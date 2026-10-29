package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/tidwall/gjson"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	upstreamAttempt := hasUpstreamExecutionAttempt(err)
	err = unwrapUpstreamExecutionAttempt(err)
	bootstrapErr := &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
	if upstreamAttempt {
		return markUpstreamExecutionAttempt(bootstrapErr)
	}
	return bootstrapErr
}

func (m *Manager) firstTokenTimeoutDuration(provider string) time.Duration {
	if m == nil {
		return 0
	}
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil {
		return 0
	}
	// Provider-specific override takes precedence
	if pCfg, ok := cfg.Routing.Providers[strings.ToLower(strings.TrimSpace(provider))]; ok {
		if raw := strings.TrimSpace(pCfg.FirstTokenTimeout); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				return d
			}
		}
	}
	// Global routing fallback
	if raw := strings.TrimSpace(cfg.Routing.FirstTokenTimeout); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

const (
	// firstTokenPer1kPromptTokens widens the first-token ceiling for large prompts.
	// Long-context reasoning legitimately needs well over the base budget before the
	// first chunk arrives; failing such a request over burns a healthy credential and
	// still returns nothing.
	firstTokenPer1kPromptTokens = 100 * time.Millisecond
	// firstTokenMaxExtraBudget caps the widening so a huge prompt cannot hide a
	// genuinely dead credential indefinitely.
	firstTokenMaxExtraBudget = 120 * time.Second
	// estimatedBytesPerToken is a conservative (overestimating) text ratio.
	estimatedBytesPerToken = 3
	// defaultStreamIdleTimeout aborts a stream that stops producing chunks mid-flight.
	// It is deliberately generous: only a true hang should be cut, never a slow model.
	defaultStreamIdleTimeout = 180 * time.Second
)

// effectiveFirstTokenTimeout scales the configured base ceiling with the reasoning
// level of the requested model and with prompt size.
//
// Scaling only ever raises the ceiling: the result is never below the configured base,
// so it cannot introduce failovers that the base value would not already produce.
// Reasoning models and long prompts legitimately need more time before the first
// chunk; cutting them off would burn a healthy credential and still return nothing.
func (m *Manager) effectiveFirstTokenTimeout(provider, model string, payload []byte) time.Duration {
	base := m.firstTokenTimeoutDuration(provider)
	if base <= 0 {
		return 0
	}
	scaled := time.Duration(float64(base) * firstTokenReasoningMultiplier(model, payload))
	extra := time.Duration(estimatePromptTokens(payload)/1000) * firstTokenPer1kPromptTokens
	if extra > firstTokenMaxExtraBudget {
		extra = firstTokenMaxExtraBudget
	}
	return scaled + extra
}

// firstTokenReasoningMultiplier widens the deadline for models that think before they
// answer. Detection is deliberately conservative: an unrecognised model keeps the base
// budget, and every recognised level only adds time.
func firstTokenReasoningMultiplier(model string, payload []byte) float64 {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, token := range []string{"-max", "-xhigh", "-high", "-thinking", "-tiered", "-pro"} {
		if strings.Contains(name, token) {
			return 1.5
		}
	}
	effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "reasoning_effort").String()))
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "reasoning.effort").String()))
	}
	switch effort {
	case "high", "xhigh", "max":
		return 1.5
	case "medium":
		return 1.25
	}
	if strings.Contains(name, "-medium") {
		return 1.25
	}
	return 1
}

// estimatePromptTokens returns a deliberately generous token estimate for a request body.
// Overestimating only widens the deadline; underestimating would reintroduce false
// first-token failovers, so text parts are counted with a low bytes-per-token ratio.
func estimatePromptTokens(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	var chars int64
	addText := func(v gjson.Result) {
		if v.Type == gjson.String {
			chars += int64(len(v.Str))
		}
	}
	for _, key := range []string{"messages", "input", "contents"} {
		root := gjson.GetBytes(payload, key)
		if !root.IsArray() {
			continue
		}
		root.ForEach(func(_, item gjson.Result) bool {
			if !item.IsObject() {
				return true
			}
			item.ForEach(func(field, value gjson.Result) bool {
				switch field.Str {
				case "content", "parts", "text":
					if value.IsArray() {
						value.ForEach(func(_, part gjson.Result) bool {
							if part.IsObject() {
								addText(part.Get("text"))
							} else {
								addText(part)
							}
							return true
						})
						return true
					}
					addText(value)
				}
				return true
			})
			return true
		})
	}
	addText(gjson.GetBytes(payload, "system"))
	addText(gjson.GetBytes(payload, "instructions"))
	chars += int64(len(gjson.GetBytes(payload, "tools").Raw))
	return int(chars / estimatedBytesPerToken)
}

// streamIdleTimeout resolves the post-first-chunk silence ceiling. An explicit "0s"
// disables the watchdog; an unparsable value falls back to the default instead of
// silently disabling protection.
func (m *Manager) streamIdleTimeout() time.Duration {
	if m == nil {
		return defaultStreamIdleTimeout
	}
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil {
		return defaultStreamIdleTimeout
	}
	raw := strings.TrimSpace(cfg.Routing.StreamIdleTimeout)
	if raw == "" {
		return defaultStreamIdleTimeout
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return defaultStreamIdleTimeout
	}
	if parsed <= 0 {
		return 0
	}
	return parsed
}

// hasFailoverCandidate reports whether a stalled attempt on auth can be replaced
// by another usable credential for the same route model.
//
// The first-token timeout is only a failover trigger. When it fires and no
// alternative credential exists, cancelling the attempt would convert a slow but
// valid upstream response into a hard 504, so callers must skip enforcement in
// that case and let the upstream finish.
func (m *Manager) hasFailoverCandidate(auth *Auth, provider, routeModel string) bool {
	if m == nil || auth == nil {
		return false
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		providerKey = canonicalSchedulingProvider(provider)
	}
	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled || candidate.ID == auth.ID {
			continue
		}
		if executorKeyFromAuth(candidate) != providerKey {
			continue
		}
		candidates = append(candidates, candidate)
	}
	m.mu.RUnlock()
	if len(candidates) == 0 {
		return false
	}
	registryRef := registry.GetGlobalRegistry()
	now := time.Now()
	for _, candidate := range candidates {
		if routeModel != "" && !m.authSupportsRouteModel(registryRef, candidate, routeModel) {
			continue
		}
		checkModel := m.selectionModelForAuth(candidate, routeModel)
		if blocked, _, _ := isAuthBlockedForModel(candidate, checkModel, now); blocked {
			continue
		}
		return true
	}
	return false
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func validateStreamResult(result *cliproxyexecutor.StreamResult, err error) (*cliproxyexecutor.StreamResult, error) {
	if err != nil {
		return result, err
	}
	if result == nil || result.Chunks == nil {
		return result, &Error{Code: "empty_stream", Message: "upstream stream has no source", Retryable: true}
	}
	return result, nil
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			// Prefer a chunk that is already available so an upstream error delivered together
			// with a context cancellation is not discarded.
			select {
			case chunk, ok = <-ch:
			default:
				select {
				case <-ctx.Done():
					return nil, false, ctx.Err()
				case chunk, ok = <-ch:
				}
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

// recordCanceledUpstreamCooldown keeps an upstream-confirmed cooldown (an error carrying
// RetryAfter) when the request context was canceled while the executor was returning, for
// example because the downstream connection was closed in response to the same upstream error.
// Errors without RetryAfter, such as plain client cancellations, are not recorded.
func (m *Manager) recordCanceledUpstreamCooldown(ctx context.Context, auth *Auth, provider, resultModel, routeModel string, opts cliproxyexecutor.Options, err error, ephemeral bool, executionSeq uint64) {
	retryAfter := retryAfterFromError(err)
	if m == nil || auth == nil || retryAfter == nil {
		return
	}
	action, okAction := matchRequestScopedErrorAction(auth, err, m.runtimeConfigSnapshot())
	result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(err), Options: opts, RetryAfter: retryAfter, AccessToken: authAccessTokenForRequest(ctx, auth, opts.Headers), CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
	result.CredentialScope = isCredentialScopedError(err)
	applyRequestScopedActionToResult(action, okAction, &result)
	m.recordExecutionResult(context.WithoutCancel(ctx), result, auth, ephemeral)
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel, routeModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, aliasResult OAuthModelAliasResult, ephemeralResult bool, opts cliproxyexecutor.Options, executionSeq uint64, abort func()) *cliproxyexecutor.StreamResult {
	ctx = WithCodexClientRequest(ctx, IsCodexClientRequest(ctx, opts.Headers))
	accessToken := authAccessTokenForRequest(ctx, auth, opts.Headers)
	out := make(chan cliproxyexecutor.StreamChunk)
	streamStart := time.Now()
	go func() {
		defer close(out)
		var failed bool
		forward := true
		var rewriter *StreamRewriter
		if aliasResult.ForceMapping && strings.TrimSpace(aliasResult.OriginalAlias) != "" {
			rewriter = NewStreamRewriter(StreamRewriteOptions{RewriteModel: aliasResult.OriginalAlias})
		}
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				entry := logEntryWithRequestID(ctx)
				warnLogUpstreamFailure(ctx, entry, provider, resultModel, auth, time.Since(streamStart), chunk.Err)
				rerr := resultErrorFromError(chunk.Err)
				action, okAction := matchRequestScopedErrorAction(auth, chunk.Err, m.runtimeConfigSnapshot())
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: opts, AccessToken: accessToken, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(chunk.Err)
				result.CredentialScope = isCredentialScopedError(chunk.Err)
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			}
			if !forward {
				return false
			}
			if chunk.Err != nil {
				if ctx == nil {
					out <- chunk
					return true
				}
				select {
				case <-ctx.Done():
					forward = false
					return false
				case out <- chunk:
					return true
				}
			}
			if len(chunk.Payload) == 0 {
				return true
			}
			payload := rewriteForceMappedStreamChunk(rewriter, chunk.Payload)
			if len(payload) == 0 {
				return true
			}
			chunk.Payload = payload
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if !drainRemaining(remaining, m.streamIdleTimeout(), abort, emit) {
			discardStreamChunks(remaining)
			return
		}
		if tail := finishForceMappedStreamChunks(rewriter); len(tail) > 0 {
			tailChunk := cliproxyexecutor.StreamChunk{Payload: tail}
			if !emit(tailChunk) {
				return
			}
		}
		if !failed && (ephemeralResult || claudeOAuthRequestCancellation(ctx, auth, nil) == nil) {
			m.recordExecutionResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: true, Options: opts, AccessToken: accessToken, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}, auth, ephemeralResult)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

// drainRemaining forwards upstream chunks until the channel closes, aborting a
// stream that stays silent longer than idle.
//
// A silent upstream would otherwise leave the client waiting until its own
// timeout while the credential keeps a live connection. The abort is surfaced as
// a stream error and never retried, because payload bytes already reached the
// downstream. It returns false when the downstream disappeared, which tells the
// caller to stop forwarding.
func drainRemaining(remaining <-chan cliproxyexecutor.StreamChunk, idle time.Duration, abort func(), emit func(cliproxyexecutor.StreamChunk) bool) bool {
	if idle <= 0 {
		for chunk := range remaining {
			if !emit(chunk) {
				return false
			}
		}
		return true
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case chunk, ok := <-remaining:
			if !ok {
				return true
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
			if !emit(chunk) {
				return false
			}
		case <-timer.C:
			if abort != nil {
				abort()
			}
			emit(cliproxyexecutor.StreamChunk{Err: &Error{
				Code:       ErrorCodeStreamIdleTimeout,
				Message:    fmt.Sprintf("stream idle timeout: no upstream data for %s", idle),
				HTTPStatus: http.StatusGatewayTimeout,
			}})
			return false
		}
	}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel, executionModel string, execModels []string, pooled bool, aliasResult OAuthModelAliasResult, routing *apiKeyModelRoutingSnapshot, allowRetry bool, ephemeralResult bool) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	executor = executorForAuth(executor, auth)
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	var lastErr error
	var upstreamErr error
	didRefreshOnUnauthorized := false
	for idx, execModel := range execModels {
		ctx = newUpstreamAttemptContext(ctx)
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		if executionModel != "" {
			execReq.Model = executionModel
		}
		execOpts := opts
		var errIntercept error
		execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(ctx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
		if errIntercept != nil {
			return nil, errIntercept
		}
		execReq = attachResolvedExecutionModelInfo(routing, execReq, auth, routeModel, execModel, executionModel != "")
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		entry := logEntryWithRequestID(ctx)
		payload := execOpts.OriginalRequest
		if len(payload) == 0 {
			payload = execReq.Payload
		}
		execOpts.Metadata = ensureCanonicalSessionMetadata(execOpts.Metadata, execOpts.Headers, payload)
		ctx = syncMetadataSessionToContext(ctx, execOpts.Metadata)
		executionSeq := m.nextExecutionSeq()
		startStream := time.Now()
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		// The attempt context must outlive this call while a stream is running, so it is
		// released when the request context ends rather than at return. Cancelling twice
		// (timer plus request end) is safe.
		context.AfterFunc(ctx, cancelAttempt)
		var timedOut atomic.Bool
		var timer *time.Timer
		failoverAvailable := m.hasFailoverCandidate(auth, provider, routeModel)
		firstTokenDeadline := m.effectiveFirstTokenTimeout(provider, routeModel, payload)
		if firstTokenDeadline > 0 && failoverAvailable && ctx != nil {
			timer = time.AfterFunc(firstTokenDeadline, func() {
				timedOut.Store(true)
				cancelAttempt()
			})
		}
		streamResult, errStream := executor.ExecuteStream(attemptCtx, auth, execReq, execOpts)
		errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
		if hasUpstreamExecutionAttempt(errStream) {
			upstreamErr = errStream
		}
		durationStream := time.Since(startStream)
		if errStream != nil && timedOut.Load() && (ctx == nil || ctx.Err() == nil) {
			errStream = &Error{
				Code:       ErrorCodeFirstTokenTimeout,
				Message:    "first-token timeout: upstream did not respond within configured limit",
				Retryable:  true,
				HTTPStatus: http.StatusGatewayTimeout,
			}
		}
		if errStream != nil && !timedOut.Load() && strings.TrimSpace(auth.ProxyPool) != "" && m.rtProvider != nil {
			proxyLimit := 1
			if lp, ok := m.rtProvider.(interface{ ProxyPoolAttemptLimit(*Auth) int }); ok {
				proxyLimit = lp.ProxyPoolAttemptLimit(auth)
				if proxyLimit > 4 {
					proxyLimit = 4
				}
			}
			for pAttempt := 1; pAttempt < proxyLimit; pAttempt++ {
				status := statusCodeFromError(errStream)
				isRetryable := status == http.StatusForbidden ||
					status == http.StatusTooManyRequests ||
					status == http.StatusBadGateway ||
					status == http.StatusServiceUnavailable ||
					status == http.StatusGatewayTimeout ||
					isTransientTransportError(errStream)
				if !isRetryable || ctx.Err() != nil {
					break
				}
				if rt := m.roundTripperFor(auth); rt != nil {
					attemptCtx = context.WithValue(attemptCtx, roundTripperContextKey{}, rt)
					attemptCtx = context.WithValue(attemptCtx, "cliproxy.roundtripper", rt)
				}
				startRetry := time.Now()
				streamResult, errStream = executor.ExecuteStream(attemptCtx, auth, execReq, execOpts)
				errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
				if hasUpstreamExecutionAttempt(errStream) {
					upstreamErr = errStream
				}
				durationStream = time.Since(startRetry)
				if errStream == nil {
					break
				}
			}
		}
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				m.recordCanceledUpstreamCooldown(ctx, auth, provider, resultModel, routeModel, execOpts, errStream, ephemeralResult, executionSeq)
				return nil, errCtx
			}
			if allowRetry && !ephemeralResult {
				alreadyTried := didRefreshOnUnauthorized
				refreshed, okRefresh := m.tryRefreshAfterUnauthorized(newUpstreamAttemptContext(ctx), auth, errStream, alreadyTried, execOpts.Headers)
				if okRefresh {
					auth = refreshed
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					if executionModel == "" {
						if rebuilt, _, changed := m.rebuildRequestAfterRefresh(auth, routeModel, execReq.Model, execReq); changed {
							execReq = rebuilt
						}
					}
					ctx = newUpstreamAttemptContext(ctx)
					ctx = syncMetadataSessionToContext(ctx, execOpts.Metadata)
					executionSeq = m.nextExecutionSeq()
					startRetry := time.Now()
					streamResult, errStream = executor.ExecuteStream(ctx, auth, execReq, execOpts)
					errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
					if hasUpstreamExecutionAttempt(errStream) {
						upstreamErr = errStream
					}
					durationRetry := time.Since(startRetry)
					if errStream != nil {
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationRetry, errStream)
						if errCtx := ctx.Err(); errCtx != nil {
							m.recordCanceledUpstreamCooldown(ctx, auth, provider, resultModel, routeModel, execOpts, errStream, ephemeralResult, executionSeq)
							return nil, errCtx
						}
					}
				} else {
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationStream, errStream)
				}
			} else {
				warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationStream, errStream)
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, errStream); errCancel != nil {
				return nil, errCancel
			}
		}
		streamResult, errStream = validateStreamResult(streamResult, errStream)
		errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
		if errStream != nil {
			rerr := resultErrorFromError(errStream)
			action, okAction := matchRequestScopedErrorAction(auth, errStream, m.runtimeConfigSnapshot())
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			result.RetryAfter = retryAfterFromError(errStream)
			if isCredentialScopedError(errStream) {
				result.CredentialScope = true
			}
			applyRequestScopedActionToResult(action, okAction, &result)
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			errStream = annotateTrustedRetryAfterByHint(errStream)
			syncPreferredUpstreamError(&upstreamErr, errStream)
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(errStream)
				}
				lastErr = errStream
				if result.CredentialScope {
					return nil, preferredExecutionAttemptError(errStream, upstreamErr)
				}
				continue
			}
			if isRequestInvalidError(errStream) || cliproxyexecutor.IsUpstreamWebsocketTurnError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			if result.CredentialScope {
				return nil, preferredExecutionAttemptError(errStream, upstreamErr)
			}
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(attemptCtx, streamResult.Chunks)
		if timer != nil {
			timer.Stop()
		}
		if bootstrapErr != nil && timedOut.Load() && (ctx == nil || ctx.Err() == nil) {
			bootstrapErr = &Error{
				Code:       ErrorCodeFirstTokenTimeout,
				Message:    "first-token timeout: upstream credential did not produce initial token within configured limit",
				Retryable:  true,
				HTTPStatus: http.StatusGatewayTimeout,
			}
		}
		bootstrapErr = markUpstreamExecutionAttemptFromContext(ctx, bootstrapErr)
		if hasUpstreamExecutionAttempt(bootstrapErr) {
			upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				m.recordCanceledUpstreamCooldown(ctx, auth, provider, resultModel, routeModel, execOpts, bootstrapErr, ephemeralResult, executionSeq)
				discardStreamChunks(streamResult.Chunks)
				return nil, errCtx
			}
			if allowRetry && !ephemeralResult {
				alreadyTried := didRefreshOnUnauthorized
				refreshed, okRefresh := m.tryRefreshAfterUnauthorized(newUpstreamAttemptContext(ctx), auth, bootstrapErr, alreadyTried, execOpts.Headers)
				if okRefresh {
					discardStreamChunks(streamResult.Chunks)
					auth = refreshed
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					if executionModel == "" {
						if rebuilt, _, changed := m.rebuildRequestAfterRefresh(auth, routeModel, execReq.Model, execReq); changed {
							execReq = rebuilt
						}
					}
					ctx = newUpstreamAttemptContext(ctx)
					executionSeq = m.nextExecutionSeq()
					startRetry := time.Now()
					retryAttemptCtx, cancelRetryAttempt := context.WithCancel(ctx)
					context.AfterFunc(ctx, cancelRetryAttempt)
					var retryTimedOut atomic.Bool
					var retryTimer *time.Timer
					if firstTokenDeadline > 0 && failoverAvailable && ctx != nil {
						retryTimer = time.AfterFunc(firstTokenDeadline, func() {
							retryTimedOut.Store(true)
							cancelRetryAttempt()
						})
					}
					retryStream, retryErr := executor.ExecuteStream(retryAttemptCtx, auth, execReq, execOpts)
					retryErr = markUpstreamExecutionAttemptFromContext(ctx, retryErr)
					if retryErr != nil && retryTimedOut.Load() && (ctx == nil || ctx.Err() == nil) {
						retryErr = &Error{
							Code:       ErrorCodeFirstTokenTimeout,
							Message:    "first-token timeout: upstream did not respond within configured limit",
							Retryable:  true,
							HTTPStatus: http.StatusGatewayTimeout,
						}
					}
					retryStream, retryErr = validateStreamResult(retryStream, retryErr)
					retryErr = markUpstreamExecutionAttemptFromContext(ctx, retryErr)
					if retryErr != nil {
						if retryTimer != nil {
							retryTimer.Stop()
						}
						if errCtx := ctx.Err(); errCtx != nil {
							m.recordCanceledUpstreamCooldown(ctx, auth, provider, resultModel, routeModel, execOpts, retryErr, ephemeralResult, executionSeq)
							return nil, errCtx
						}
						bootstrapErr = retryErr
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), bootstrapErr)
						streamResult = &cliproxyexecutor.StreamResult{}
					} else {
						streamResult = retryStream
						buffered, closed, bootstrapErr = readStreamBootstrap(retryAttemptCtx, streamResult.Chunks)
						if retryTimer != nil {
							retryTimer.Stop()
						}
						if bootstrapErr != nil && retryTimedOut.Load() && (ctx == nil || ctx.Err() == nil) {
							bootstrapErr = &Error{
								Code:       ErrorCodeFirstTokenTimeout,
								Message:    "first-token timeout: upstream credential did not produce initial token within configured limit",
								Retryable:  true,
								HTTPStatus: http.StatusGatewayTimeout,
							}
						}
						bootstrapErr = markUpstreamExecutionAttemptFromContext(ctx, bootstrapErr)
						if bootstrapErr != nil {
							warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), bootstrapErr)
						}
					}
				} else {
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), bootstrapErr)
				}
			} else {
				warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), bootstrapErr)
			}
			if hasUpstreamExecutionAttempt(bootstrapErr) {
				upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, bootstrapErr); errCancel != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCancel
			}
		}
		if bootstrapErr != nil {
			action, okAction := matchRequestScopedErrorAction(auth, bootstrapErr, m.runtimeConfigSnapshot())
			if okAction {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				bootstrapErr = annotateTrustedRetryAfterByHint(bootstrapErr)
				bootstrapErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
				syncPreferredUpstreamError(&upstreamErr, bootstrapErr)
				discardStreamChunks(streamResult.Chunks)
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(bootstrapErr)
				}
				lastErr = bootstrapErr
				if result.CredentialScope {
					currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
					return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
				}
				continue
			}
			if isRequestInvalidError(bootstrapErr) || cliproxyexecutor.IsUpstreamWebsocketTurnError(bootstrapErr) {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				bootstrapErr = annotateTrustedRetryAfterByHint(bootstrapErr)
				syncPreferredUpstreamError(&upstreamErr, bootstrapErr)
				discardStreamChunks(streamResult.Chunks)
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				bootstrapErr = annotateTrustedRetryAfterByHint(bootstrapErr)
				bootstrapErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
				syncPreferredUpstreamError(&upstreamErr, bootstrapErr)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				if result.CredentialScope {
					currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
					return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
				}
				continue
			}
			rerr := resultErrorFromError(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			if isCredentialScopedError(bootstrapErr) {
				result.CredentialScope = true
			}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			bootstrapErr = annotateTrustedRetryAfterByHint(bootstrapErr)
			bootstrapErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			syncPreferredUpstreamError(&upstreamErr, bootstrapErr)
			discardStreamChunks(streamResult.Chunks)
			currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
		}

		if closed && len(buffered) == 0 {
			emptyErr := markUpstreamExecutionAttemptFromContext(ctx, &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true})
			currentErr := newStreamBootstrapError(emptyErr, streamResult.Headers)
			if hasUpstreamExecutionAttempt(emptyErr) {
				upstreamErr = currentErr
			}
			warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), emptyErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(emptyErr), Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		attemptAliasResult := resolveAttemptAliasResult(routing, auth, routeModel, execModel, aliasResult)
		return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, routeModel, streamResult.Headers, buffered, remaining, attemptAliasResult, ephemeralResult, execOpts, executionSeq, cancelAttempt), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, preferredExecutionAttemptError(lastErr, upstreamErr)
}
