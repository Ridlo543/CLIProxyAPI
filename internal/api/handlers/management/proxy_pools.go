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

func sanitizeProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "direct") {
		return "direct"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return parsed.Redacted()
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
