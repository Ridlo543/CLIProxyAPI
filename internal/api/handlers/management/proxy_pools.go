package management

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type proxyPoolReporter interface {
	ProxyPoolStatuses() []coreauth.ProxyPoolStatus
}

// sanitizeProxyURL reduces a pool entry to the endpoint the operator needs to
// recognise it: scheme and host only. url.Redacted() masks the password but
// still hands back the username, path and query, and a proxy URL routinely
// carries the account id or an auth token in exactly those places.
func sanitizeProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "direct") {
		return "direct"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		// Unparseable entries are never echoed back; the name and counts
		// already tell the operator which pool is misconfigured.
		return "invalid"
	}
	if parsed.Scheme == "" {
		return parsed.Host
	}
	return parsed.Scheme + "://" + parsed.Host
}

// GetProxyPools returns the sanitized status of all configured proxy pools.
func (h *Handler) GetProxyPools(c *gin.Context) {
	var pools []coreauth.ProxyPoolStatus
	if h != nil && h.authManager != nil {
		if reporter, ok := h.authManager.RoundTripperProvider().(proxyPoolReporter); ok {
			pools = reporter.ProxyPoolStatuses()
		}
	}

	sanitized := make([]coreauth.ProxyPoolStatus, len(pools))
	for i, p := range pools {
		urls := make([]string, len(p.URLs))
		for j, u := range p.URLs {
			urls[j] = sanitizeProxyURL(u)
		}
		sanitized[i] = coreauth.ProxyPoolStatus{
			Name:       p.Name,
			Strategy:   p.Strategy,
			Strict:     p.Strict,
			EntryCount: p.EntryCount,
			Healthy:    p.Healthy,
			URLs:       urls,
		}
	}

	sort.Slice(sanitized, func(i, j int) bool {
		return sanitized[i].Name < sanitized[j].Name
	})

	c.JSON(http.StatusOK, gin.H{"proxy-pools": sanitized})
}
