package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

type websocketProxyAttempt struct{ failures atomic.Int32 }

func (*websocketProxyAttempt) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
func (*websocketProxyAttempt) ProxyWebsocketDialConfig() (proxyutil.WebsocketDialConfig, error) {
	return proxyutil.WebsocketDialConfig{Direct: true}, nil
}
func (a *websocketProxyAttempt) ReportProxyTransportFailure() { a.failures.Add(1) }

func TestCodexAndXAIWebsocketUseSelectedDirectAndReportHandshakeFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "not websocket", http.StatusBadRequest)
	}))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.invalid:1"}}
	auth := &cliproxyauth.Auth{ProxyPool: "office"}

	for _, test := range []struct {
		name string
		dial func(context.Context) error
	}{
		{name: "codex", dial: func(ctx context.Context) error {
			_, _, resp, err := NewCodexWebsocketsExecutor(cfg).dialCodexWebsocket(ctx, auth, wsURL, nil)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			return err
		}},
		{name: "xai", dial: func(ctx context.Context) error {
			_, _, resp, err := NewXAIWebsocketsExecutor(cfg).dialXAIWebsocket(ctx, auth, wsURL, nil)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempt := &websocketProxyAttempt{}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(attempt))
			if err := test.dial(ctx); err == nil {
				t.Fatal("expected handshake failure")
			}
			if attempt.failures.Load() != 1 {
				t.Fatalf("failure reports = %d", attempt.failures.Load())
			}
		})
	}
	if requests.Load() != 2 {
		t.Fatalf("direct server requests = %d, want 2; global proxy may have won", requests.Load())
	}
}
