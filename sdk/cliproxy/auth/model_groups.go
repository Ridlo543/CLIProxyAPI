package auth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/sjson"
)

// ModelGroupTarget identifies one ordered provider/model target.
type ModelGroupTarget struct {
	Provider string
	Model    string
}

func setModelInPayload(payload []byte, model string) []byte {
	if len(payload) == 0 {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "model", model)
	if err != nil {
		return payload
	}
	return updated
}

// ExecuteCountModelGroup counts tokens using ordered fallback targets.
func (m *Manager) ExecuteCountModelGroup(ctx context.Context, targets []ModelGroupTarget, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	var lastErr error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return cliproxyexecutor.Response{}, err
		}
		memberReq := req
		memberReq.Model = target.Model
		memberReq.Payload = setModelInPayload(req.Payload, target.Model)
		resp, err := m.ExecuteCount(ctx, []string{target.Provider}, memberReq, opts)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isModelGroupFallbackError(err) {
			return cliproxyexecutor.Response{}, err
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "model group has no targets"}
}

// ExecuteModelGroup executes targets in order, preserving normal Manager retry semantics per target.
func (m *Manager) ExecuteModelGroup(ctx context.Context, targets []ModelGroupTarget, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	var lastErr error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return cliproxyexecutor.Response{}, err
		}
		memberReq := req
		memberReq.Model = target.Model
		memberReq.Payload = setModelInPayload(req.Payload, target.Model)
		resp, err := m.Execute(ctx, []string{target.Provider}, memberReq, opts)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isModelGroupFallbackError(err) {
			return cliproxyexecutor.Response{}, err
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "model group has no targets"}
}

// ExecuteStreamModelGroup falls back only when stream setup returns an error.
func (m *Manager) ExecuteStreamModelGroup(ctx context.Context, targets []ModelGroupTarget, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	var lastErr error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		memberReq := req
		memberReq.Model = target.Model
		memberReq.Payload = setModelInPayload(req.Payload, target.Model)
		result, err := m.ExecuteStream(ctx, []string{target.Provider}, memberReq, opts)
		if err == nil {
			// ExecuteStream reports a failure that happened before the first
			// payload as a *successful* result whose only chunk carries the error,
			// so HTTP handlers can still surface the upstream headers. To this loop
			// that looked like success, and the group stopped at its first member:
			// a 503 capacity rejection on target one never fell through to target
			// two. Unwrap it back into an error so fallback can decide.
			bootstrapErr, forwarded := modelGroupStreamBootstrapFailure(ctx, result)
			if bootstrapErr == nil {
				return forwarded, nil
			}
			err = bootstrapErr
		}
		lastErr = err
		if !isModelGroupFallbackError(err) {
			return modelGroupStreamFailure(err)
		}
	}
	if lastErr != nil {
		return modelGroupStreamFailure(lastErr)
	}
	return nil, &Error{Code: "provider_not_found", Message: "model group has no targets"}
}

// modelGroupStreamBootstrapFailure inspects a result ExecuteStream reported as
// successful. A bootstrap failure is delivered as a buffered, already-closed
// channel holding one error chunk, so it is always readable straight away; a
// live upstream stream normally is not, and is handed back untouched. When a
// chunk is read from a live stream it is replayed so nothing is swallowed.
func modelGroupStreamBootstrapFailure(ctx context.Context, result *cliproxyexecutor.StreamResult) (error, *cliproxyexecutor.StreamResult) {
	if result == nil || result.Chunks == nil {
		return nil, result
	}
	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			return nil, result
		}
		var bootstrapErr *streamBootstrapError
		if chunk.Err != nil && len(chunk.Payload) == 0 && errors.As(chunk.Err, &bootstrapErr) {
			return chunk.Err, nil
		}
		return nil, replayStreamResult(ctx, result, chunk)
	default:
		return nil, result
	}
}

// replayStreamResult puts an already-read chunk back at the head of the stream.
func replayStreamResult(ctx context.Context, result *cliproxyexecutor.StreamResult, first cliproxyexecutor.StreamChunk) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		select {
		case out <- first:
		case <-ctx.Done():
			return
		}
		for chunk := range result.Chunks {
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}
}

// modelGroupStreamFailure keeps the contract ExecuteStream established: a
// bootstrap failure travels back as an error chunk so the upstream headers
// reach the client, anything else as a plain error.
func modelGroupStreamFailure(err error) (*cliproxyexecutor.StreamResult, error) {
	var bootstrapErr *streamBootstrapError
	if errors.As(err, &bootstrapErr) && bootstrapErr != nil {
		return streamErrorResult(bootstrapErr.Headers(), err), nil
	}
	return nil, err
}

func isModelGroupFallbackError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isRequestInvalidError(err) || isRequestStopError(err) {
		return false
	}
	status := statusCodeFromError(err)
	if status == 0 && isTransientModelGroupNetworkError(err) {
		return true
	}
	var authErr *Error
	typedAuthFailure := errors.As(err, &authErr) && authErr != nil && isModelGroupAuthFailureCode(authErr.Code)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return authErr != nil && (strings.TrimSpace(authErr.Code) == "" || typedAuthFailure || authErr.Retryable)
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case 0:
		return authErr != nil && (authErr.Retryable || typedAuthFailure || authErr.Code == "provider_not_found" || authErr.Code == "executor_not_found")
	default:
		return false
	}
}

func isTransientModelGroupNetworkError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isConnectionLifecycleError(err) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func isModelGroupAuthFailureCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "auth_not_found", "auth_unavailable", "auth_expired", "invalid_grant", "token_expired", "unauthorized", "provider_not_found", "executor_not_found", "auth_disabled", "token_refresh_failed", "upstream_error", "rate_limit", "quota_exceeded", "capacity_exhausted", "service_unavailable":
		return true
	default:
		return false
	}
}
