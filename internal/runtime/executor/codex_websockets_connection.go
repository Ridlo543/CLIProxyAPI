package executor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
	codexResponsesWebsocketIdleTimeout     = 5 * time.Minute
	codexResponsesWebsocketHandshakeTO     = 30 * time.Second
)

func (e *CodexWebsocketsExecutor) dialCodexWebsocket(ctx context.Context, auth *cliproxyauth.Auth, wsURL string, headers http.Header) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
	dialer, selectedAttempt := newProxyAwareWebsocketDialer(ctx, e.cfg, auth)
	dialer.HandshakeTimeout = codexResponsesWebsocketHandshakeTO
	dialer.EnableCompression = true
	if ctx == nil {
		ctx = context.Background()
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil && selectedAttempt != nil {
		selectedAttempt.ReportProxyTransportFailure()
	}
	closer := newWebsocketConnectionCloser(conn)
	if conn != nil {
		// Avoid gorilla/websocket flate tail validation issues on some upstreams/Go versions.
		// Negotiating permessage-deflate is fine; we just don't compress outbound messages.
		conn.EnableWriteCompression(false)
	}
	return conn, closer, resp, err
}

func writeCodexWebsocketMessage(sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	if sess != nil {
		return sess.writeMessage(conn, websocket.TextMessage, payload)
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func mapCodexWebsocketWriteError(sess *codexWebsocketSession, conn *websocket.Conn, err error) error {
	if err == nil || sess == nil || conn == nil {
		return err
	}
	upstreamErr := sess.upstreamDisconnectError(conn)
	var closeErr *websocket.CloseError
	if !errors.As(upstreamErr, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		return err
	}
	return mapCodexWebsocketReadError(upstreamErr)
}

func shouldRetryCodexWebsocketSend(err error) bool {
	if err == nil {
		return false
	}
	var requestErr cliproxyexecutor.RequestScopedError
	return !errors.As(err, &requestErr) || !requestErr.IsRequestScoped()
}

type codexWebsocketMessageTooBigError struct {
	statusErr
}

func (codexWebsocketMessageTooBigError) IsRequestScoped() bool {
	return true
}

func mapCodexWebsocketReadError(err error) error {
	if err == nil {
		return nil
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseMessageTooBig {
		return codexWebsocketMessageTooBigError{statusErr: statusErr{
			code: http.StatusRequestEntityTooLarge,
			msg:  `{"error":{"message":"upstream websocket message too big","type":"invalid_request_error","code":"message_too_big"}}`,
		}}
	}
	return err
}

func normalizeCodexWebsocketParallelToolCalls(body []byte, headers http.Header) []byte {
	if !isCodexResponsesLiteRequest(body, headers) {
		return body
	}
	body = helps.SetBoolIfDifferent(body, "parallel_tool_calls", false)
	return body
}

func buildCodexWebsocketRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}

	// Match codex-rs websocket v2 semantics: every request is `response.create`.
	// Incremental follow-up turns continue on the same websocket using
	// `previous_response_id` + incremental `input`, not `response.append`.
	body = helps.SanitizeCodexInputItemIDs(body)
	wsReqBody, errSet := sjson.SetBytes(body, "type", "response.create")
	if errSet == nil && len(wsReqBody) > 0 {
		return wsReqBody
	}
	return body
}

func readCodexWebsocketMessage(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) (int, []byte, error) {
	if sess == nil {
		if conn == nil {
			return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
		}
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		return msgType, payload, errRead
	}
	if conn == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	if readCh == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: session read channel is nil")
	}
	for {
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case ev, ok := <-readCh:
			if !ok {
				return 0, nil, fmt.Errorf("codex websockets executor: session read channel closed")
			}
			if ev.conn != conn {
				continue
			}
			if ev.err != nil {
				return 0, nil, ev.err
			}
			return ev.msgType, ev.payload, nil
		}
	}
}

type selectedWebsocketProxyAttempt interface {
	ProxyWebsocketDialConfig() (proxyutil.WebsocketDialConfig, error)
	ReportProxyTransportFailure()
}

func newProxyAwareWebsocketDialer(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*websocket.Dialer, selectedWebsocketProxyAttempt) {
	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  codexResponsesWebsocketHandshakeTO,
		EnableCompression: true,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if auth == nil || strings.TrimSpace(auth.ProxyURL) == "" {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(selectedWebsocketProxyAttempt); ok && rt != nil {
			selected, errSelected := rt.ProxyWebsocketDialConfig()
			if errSelected != nil {
				dialer.Proxy = func(*http.Request) (*url.URL, error) { return nil, errors.New("selected proxy attempt unavailable") }
				return dialer, rt
			}
			applyWebsocketDialConfig(dialer, selected)
			return dialer, rt
		}
	}

	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if proxyURL == "" {
		return dialer, nil
	}

	setting, errParse := proxyutil.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("codex websockets executor: %v", errParse)
		return dialer, nil
	}

	switch setting.Mode {
	case proxyutil.ModeDirect:
		dialer.Proxy = nil
		return dialer, nil
	case proxyutil.ModeProxy:
	default:
		return dialer, nil
	}

	switch setting.URL.Scheme {
	case "socks5", "socks5h":
		selected, errSelected := proxyutil.BuildWebsocketDialConfig(proxyURL)
		if errSelected != nil {
			return dialer, nil
		}
		applyWebsocketDialConfig(dialer, selected)
	case "http", "https":
		dialer.Proxy = http.ProxyURL(setting.URL)
	default:
		log.Errorf("codex websockets executor: unsupported proxy scheme: %s", setting.URL.Scheme)
	}

	return dialer, nil
}

func applyWebsocketDialConfig(dialer *websocket.Dialer, selected proxyutil.WebsocketDialConfig) {
	if selected.Direct {
		dialer.Proxy = nil
		return
	}
	if selected.Proxy != nil {
		dialer.Proxy = selected.Proxy
	}
	if selected.NetDialContext != nil {
		dialer.Proxy = nil
		dialer.NetDialContext = selected.NetDialContext
	}
}

func buildCodexResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("codex websockets executor: unsupported responses websocket URL scheme %q", parsed.Scheme)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("codex websockets executor: responses websocket URL host is empty")
	}
	return parsed.String(), nil
}
