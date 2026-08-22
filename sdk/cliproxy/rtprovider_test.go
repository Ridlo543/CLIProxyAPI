package cliproxy

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func testPool(strategy string, strict bool, entries ...string) *defaultRoundTripperProvider {
	poolEntries := make([]config.ProxyPoolEntry, len(entries))
	for i := range entries {
		poolEntries[i].URL = entries[i]
	}
	return newDefaultRoundTripperProvider(&config.Config{SDKConfig: config.SDKConfig{
		ProxyURL:   "http://global.example:8080",
		ProxyPools: []config.ProxyPool{{Name: "office", Strategy: strategy, Strict: strict, Entries: poolEntries}},
	}})
}

func proxyAddress(t *testing.T, rt http.RoundTripper) string {
	t.Helper()
	transport, ok := rt.(*poolRoundTripper)
	if ok {
		rt = transport.base
	}
	httpTransport, ok := rt.(*http.Transport)
	if !ok || httpTransport.Proxy == nil {
		return "direct"
	}
	req, _ := http.NewRequest(http.MethodGet, "https://upstream.example", nil)
	u, err := httpTransport.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		return "direct"
	}
	return u.Host
}

func TestProxyPoolPrecedenceRoundRobinAndDirect(t *testing.T) {
	p := testPool("round-robin", true, "http://one.example:1", "direct")
	auth := &coreauth.Auth{ProxyPool: "office"}
	if got := proxyAddress(t, p.RoundTripperFor(auth)); got != "one.example:1" {
		t.Fatalf("first = %q", got)
	}
	if got := proxyAddress(t, p.RoundTripperFor(auth)); got != "direct" {
		t.Fatalf("second = %q", got)
	}
	auth.ProxyURL = "http://override.example:9"
	if got := proxyAddress(t, p.RoundTripperFor(auth)); got != "override.example:9" {
		t.Fatalf("override = %q", got)
	}
	auth.ProxyURL, auth.ProxyPool = "", ""
	if got := proxyAddress(t, p.RoundTripperFor(auth)); got != "global.example:8080" {
		t.Fatalf("global = %q", got)
	}
}

func TestProxyPoolOrderedCooldownAndStrictErrors(t *testing.T) {
	p := testPool("ordered-fallback", true, "http://one.example:1", "http://two.example:2")
	auth := &coreauth.Auth{ProxyPool: "office"}
	rt := p.RoundTripperFor(auth).(*poolRoundTripper)
	rt.base = roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("bootstrap failed") })
	req, _ := http.NewRequest(http.MethodGet, "https://upstream.example", nil)
	_, _ = rt.RoundTrip(req)
	if got := proxyAddress(t, p.RoundTripperFor(auth)); got != "two.example:2" {
		t.Fatalf("fallback = %q", got)
	}
	p.mu.Lock()
	p.pools["office"].entries[1].cooldownUntil = p.pools["office"].entries[0].cooldownUntil
	p.mu.Unlock()
	_, err := p.RoundTripperFor(auth).RoundTrip(req)
	if err == nil || strings.Contains(err.Error(), "one.example") {
		t.Fatalf("strict error = %v", err)
	}
	auth.ProxyPool = "missing"
	if _, err = p.RoundTripperFor(auth).RoundTrip(req); err == nil {
		t.Fatal("unknown strict pool did not fail")
	}
}

func TestProxyPoolConcurrentSelection(t *testing.T) {
	p := testPool("round-robin", true, "direct", "http://two.example:2")
	auth := &coreauth.Auth{ProxyPool: "office"}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = p.RoundTripperFor(auth) }()
	}
	wg.Wait()
	p.mu.Lock()
	next := p.pools["office"].next
	p.mu.Unlock()
	if next != 100 {
		t.Fatalf("selection count = %d", next)
	}
}

func TestProxyPoolAttemptLimitIncludesExplicitGlobalFallback(t *testing.T) {
	provider := testPool("ordered-fallback", false, "http://first.example:8080", "http://second.example:8080")
	provider.global = "direct"
	auth := &coreauth.Auth{ProxyPool: "office"}
	if got := provider.ProxyPoolAttemptLimit(auth); got != 3 {
		t.Fatalf("attempt limit = %d, want 3 (two pool entries plus explicit global fallback)", got)
	}
	for attempt := 0; attempt < 2; attempt++ {
		rt := provider.RoundTripperFor(auth)
		poolAttempt, ok := rt.(*poolRoundTripper)
		if !ok {
			t.Fatalf("attempt %d transport = %T, want pool transport", attempt+1, rt)
		}
		poolAttempt.recordFailure()
	}
	globalAttempt := provider.RoundTripperFor(auth)
	if transport, ok := globalAttempt.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatalf("global fallback transport = %T, want explicit direct transport", globalAttempt)
	}

	provider.pools["office"].strict = true
	if got := provider.ProxyPoolAttemptLimit(auth); got != 2 {
		t.Fatalf("strict attempt limit = %d, want 2", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRoundTripperForDirectBypassesProxy(t *testing.T) {
	provider := newDefaultRoundTripperProvider()
	rt := provider.RoundTripperFor(&coreauth.Auth{ProxyURL: "direct"})
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", rt)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestProxyPoolExhaustedNonStrictFailsClosedWithoutGlobal(t *testing.T) {
	p := testPool("ordered-fallback", false, "http://one.example:1")
	p.global = ""
	p.pools["office"].entries[0].cooldownUntil = time.Now().Add(time.Minute)
	req, _ := http.NewRequest(http.MethodGet, "https://upstream.example", nil)
	_, err := p.RoundTripperFor(&coreauth.Auth{ProxyPool: "office"}).RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "no healthy entries") {
		t.Fatalf("error = %v", err)
	}
}

func TestProxyPoolReloadPreservesStateByEntryIdentity(t *testing.T) {
	p := testPool("round-robin", true, "http://one.example:1", "http://two.example:2")
	auth := &coreauth.Auth{ProxyPool: "office"}
	inFlight := p.RoundTripperFor(auth).(*poolRoundTripper)
	inFlight.base = roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("late failure") })
	p.mu.Lock()
	p.pools["office"].entries[1].failures = 7
	p.pools["office"].entries[1].cooldownUntil = time.Now().Add(time.Minute)
	next := p.pools["office"].next
	p.mu.Unlock()
	p.UpdateConfig(&config.Config{SDKConfig: config.SDKConfig{ProxyPools: []config.ProxyPool{{Name: " OFFICE ", Strategy: "round-robin", Strict: true, Entries: []config.ProxyPoolEntry{{URL: "http://two.example:2"}, {URL: "http://one.example:1"}}}}}})
	p.mu.Lock()
	if p.pools["office"].entries[0].failures != 7 || p.pools["office"].next != next {
		t.Fatalf("state not preserved: %#v", p.pools["office"])
	}
	p.mu.Unlock()
	req, _ := http.NewRequest(http.MethodGet, "https://upstream.example", nil)
	_, _ = inFlight.RoundTrip(req)
	p.mu.Lock()
	if got := p.pools["office"].entries[1].failures; got != 1 {
		t.Fatalf("in-flight failure applied to wrong entry: %d", got)
	}
	p.mu.Unlock()

	removed := &poolRoundTripper{
		base:     roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("removed failure") }),
		provider: p,
		pool:     "office",
		entryID:  normalizeProxyEntryID("http://one.example:1"),
	}
	p.UpdateConfig(&config.Config{SDKConfig: config.SDKConfig{ProxyPools: []config.ProxyPool{{Name: "office", Strategy: "round-robin", Strict: true, Entries: []config.ProxyPoolEntry{{URL: "direct"}}}}}})
	_, _ = removed.RoundTrip(req)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pools["office"].entries[0].failures != 0 {
		t.Fatal("removed in-flight entry mutated replacement")
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (failingBody) Close() error             { return nil }

func TestProxyPoolResponseBodyFailureUpdatesHealth(t *testing.T) {
	p := testPool("ordered-fallback", true, "direct")
	auth := &coreauth.Auth{ProxyPool: "office"}
	rt := p.RoundTripperFor(auth).(*poolRoundTripper)
	rt.base = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: failingBody{}}, nil
	})
	req, _ := http.NewRequest(http.MethodGet, "https://upstream.example", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resp.Body.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expected body error")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pools["office"].entries[0].failures != 1 || !p.pools["office"].entries[0].cooldownUntil.After(time.Now()) {
		t.Fatalf("body failure not recorded: %#v", p.pools["office"].entries[0])
	}
}
