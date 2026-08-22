package helps

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type proxyHelperRoundTripper func(*http.Request) (*http.Response, error)

func (f proxyHelperRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestNewProxyAwareHTTPClientPoolTransportBeatsGlobal(t *testing.T) {
	selected := &proxyHelperRoundTripperHolder{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(selected))
	client := NewProxyAwareHTTPClient(
		ctx,
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global.example:8080"}},
		&cliproxyauth.Auth{ProxyPool: "office"},
		0,
	)
	if client.Transport != selected {
		t.Fatalf("transport = %T, want selected pool transport", client.Transport)
	}
}

type proxyHelperRoundTripperHolder struct{}

func (*proxyHelperRoundTripperHolder) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}
