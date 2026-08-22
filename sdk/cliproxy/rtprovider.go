package cliproxy

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

type proxyPoolEntryState struct {
	id            string
	url           string
	cooldownUntil time.Time
	failures      uint64
}

type proxyPoolState struct {
	name, strategy string
	strict         bool
	cooldown       time.Duration
	next           uint64
	entries        []proxyPoolEntryState
}

// defaultRoundTripperProvider resolves credential proxy overrides and named pools.
// Pool transport failures affect subsequent outbound attempts; requests are never replayed here.
type defaultRoundTripperProvider struct {
	mu     sync.Mutex
	cache  map[string]http.RoundTripper
	pools  map[string]*proxyPoolState
	global string
}

func newDefaultRoundTripperProvider(configs ...*config.Config) *defaultRoundTripperProvider {
	p := &defaultRoundTripperProvider{cache: make(map[string]http.RoundTripper)}
	var cfg *config.Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	p.UpdateConfig(cfg)
	return p
}

func (p *defaultRoundTripperProvider) UpdateConfig(cfg *config.Config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	oldPools := p.pools
	p.global = ""
	p.pools = make(map[string]*proxyPoolState)
	if cfg == nil {
		return
	}
	p.global = strings.TrimSpace(cfg.ProxyURL)
	for i := range cfg.ProxyPools {
		definition := cfg.ProxyPools[i]
		cooldown := config.DefaultProxyPoolCooldown
		if definition.Cooldown != "" {
			if parsed, errParse := time.ParseDuration(definition.Cooldown); errParse == nil {
				cooldown = parsed
			}
		}
		poolKey := normalizePoolName(definition.Name)
		pool := &proxyPoolState{name: definition.Name, strategy: definition.Strategy, strict: definition.Strict, cooldown: cooldown}
		oldPool := oldPools[poolKey]
		if oldPool != nil {
			pool.next = oldPool.next
		}
		for _, entry := range definition.Entries {
			entryID := normalizeProxyEntryID(entry.URL)
			state := proxyPoolEntryState{id: entryID, url: strings.TrimSpace(entry.URL)}
			if oldPool != nil {
				for i := range oldPool.entries {
					if oldPool.entries[i].id == entryID {
						state.cooldownUntil = oldPool.entries[i].cooldownUntil
						state.failures = oldPool.entries[i].failures
						break
					}
				}
			}
			pool.entries = append(pool.entries, state)
		}
		p.pools[poolKey] = pool
	}
}

func normalizePoolName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func normalizeProxyEntryID(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "direct") {
		return "direct"
	}
	setting, errParse := proxyutil.Parse(raw)
	if errParse != nil || setting.URL == nil {
		return strings.ToLower(raw)
	}
	setting.URL.Scheme = strings.ToLower(setting.URL.Scheme)
	setting.URL.Host = strings.ToLower(setting.URL.Host)
	return setting.URL.String()
}

func (p *defaultRoundTripperProvider) ProxyPoolStatuses() []coreauth.ProxyPoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]coreauth.ProxyPoolStatus, 0, len(p.pools))
	for _, pool := range p.pools {
		status := coreauth.ProxyPoolStatus{Name: pool.name, Strategy: pool.strategy, Strict: pool.strict, EntryCount: len(pool.entries)}
		for i := range pool.entries {
			redacted := proxyutil.Redact(pool.entries[i].url)
			if strings.EqualFold(strings.TrimSpace(pool.entries[i].url), "direct") {
				redacted = "direct"
			}
			status.URLs = append(status.URLs, redacted)
			status.FailureCount += pool.entries[i].failures
			if pool.entries[i].cooldownUntil.After(now) {
				status.Cooling++
			} else {
				status.Healthy++
			}
		}
		out = append(out, status)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (p *defaultRoundTripperProvider) RoundTripperFor(auth *coreauth.Auth) http.RoundTripper {
	if auth == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
		return p.transportLocked(proxyURL)
	}
	poolName := normalizePoolName(auth.ProxyPool)
	if poolName == "" {
		return p.transportLocked(p.global)
	}
	pool := p.pools[poolName]
	if pool == nil {
		return errorRoundTripper{err: errors.New("selected proxy pool is not configured")}
	}
	index := selectPoolEntryLocked(pool, time.Now())
	if index < 0 {
		if p.global != "" && !pool.strict {
			return p.transportLocked(p.global)
		}
		return errorRoundTripper{err: errors.New("selected proxy pool has no healthy entries")}
	}
	rt := p.transportLocked(pool.entries[index].url)
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &poolRoundTripper{base: rt, provider: p, pool: poolName, entryID: pool.entries[index].id}
}

func (p *defaultRoundTripperProvider) ProxyPoolAttemptLimit(auth *coreauth.Auth) int {
	if auth == nil || strings.TrimSpace(auth.ProxyURL) != "" || strings.TrimSpace(auth.ProxyPool) == "" {
		return 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if pool := p.pools[normalizePoolName(auth.ProxyPool)]; pool != nil {
		limit := len(pool.entries)
		if !pool.strict && p.global != "" {
			limit++
		}
		return limit
	}
	return 1
}

func selectPoolEntryLocked(pool *proxyPoolState, now time.Time) int {
	healthy := make([]int, 0, len(pool.entries))
	for i := range pool.entries {
		if !pool.entries[i].cooldownUntil.After(now) {
			healthy = append(healthy, i)
		}
	}
	if len(healthy) == 0 {
		return -1
	}
	switch pool.strategy {
	case "round-robin":
		selected := healthy[pool.next%uint64(len(healthy))]
		pool.next++
		return selected
	case "random":
		return healthy[rand.IntN(len(healthy))]
	default:
		return healthy[0]
	}
}

func (p *defaultRoundTripperProvider) transportLocked(raw string) http.RoundTripper {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if rt := p.cache[raw]; rt != nil {
		return rt
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(raw)
	if errBuild != nil {
		log.WithError(errBuild).Errorf("failed to configure proxy %q", proxyutil.Redact(raw))
		return errorRoundTripper{err: errors.New("invalid proxy configuration")}
	}
	if transport != nil {
		p.cache[raw] = transport
	}
	return transport
}

type poolRoundTripper struct {
	base     http.RoundTripper
	provider *defaultRoundTripperProvider
	pool     string
	entryID  string
	mu       sync.Mutex
	failed   bool
}

func (r *poolRoundTripper) ProxyWebsocketDialConfig() (proxyutil.WebsocketDialConfig, error) {
	r.provider.mu.Lock()
	defer r.provider.mu.Unlock()
	pool := r.provider.pools[r.pool]
	if pool == nil {
		return proxyutil.WebsocketDialConfig{}, errors.New("selected proxy pool is unavailable")
	}
	for i := range pool.entries {
		if pool.entries[i].id == r.entryID {
			return proxyutil.BuildWebsocketDialConfig(pool.entries[i].url)
		}
	}
	return proxyutil.WebsocketDialConfig{}, errors.New("selected proxy pool entry is unavailable")
}

func (r *poolRoundTripper) ReportProxyTransportFailure() {
	r.recordFailure()
}

func (r *poolRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err != nil {
		r.recordFailure()
	} else if resp != nil && resp.Body != nil {
		resp.Body = &poolResponseBody{ReadCloser: resp.Body, owner: r}
	}
	return resp, err
}

func (r *poolRoundTripper) recordFailure() {
	r.mu.Lock()
	if r.failed {
		r.mu.Unlock()
		return
	}
	r.failed = true
	r.mu.Unlock()
	r.provider.mu.Lock()
	defer r.provider.mu.Unlock()
	if pool := r.provider.pools[r.pool]; pool != nil {
		for i := range pool.entries {
			if pool.entries[i].id == r.entryID {
				pool.entries[i].failures++
				pool.entries[i].cooldownUntil = time.Now().Add(pool.cooldown)
				return
			}
		}
	}
}

type poolResponseBody struct {
	io.ReadCloser
	owner *poolRoundTripper
}

func (b *poolResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		b.owner.recordFailure()
	}
	return n, err
}

func (b *poolResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		b.owner.recordFailure()
	}
	return err
}

func (r *poolRoundTripper) ProxyTransportFailed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

type errorRoundTripper struct{ err error }

func (r errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, r.err }

func (r errorRoundTripper) ProxyWebsocketDialConfig() (proxyutil.WebsocketDialConfig, error) {
	return proxyutil.WebsocketDialConfig{}, r.err
}

func (errorRoundTripper) ReportProxyTransportFailure() {}
