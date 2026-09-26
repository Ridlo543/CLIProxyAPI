package api

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/combos"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
)

// ConfigureAPIKeyPolicies refreshes the process-wide policy snapshot.
func ConfigureAPIKeyPolicies(cfg *config.Config) {
	if cfg == nil {
		return
	}
	apikeypolicy.Default().Replace(cfg.APIKeyPolicies)
}

func modelKnownToRegistry(model string) ([]string, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, false
	}
	var prefixProvider string
	lookupModel := model
	if prov, m, ok := strings.Cut(model, "/"); ok && prov != "" && m != "" {
		prefixProvider = strings.ToLower(strings.TrimSpace(prov))
		lookupModel = strings.TrimSpace(m)
	}
	candidates := make([]string, 0, 3)
	if prefixProvider != "" {
		candidates = append(candidates, prefixProvider)
	}
	info := registry.GetGlobalRegistry().GetModelInfo(lookupModel, "")
	if info != nil {
		for _, candidate := range []string{info.OwnedBy, info.Type} {
			if trimmed := strings.ToLower(strings.TrimSpace(candidate)); trimmed != "" {
				candidates = append(candidates, trimmed)
			}
		}
	}
	if cmb, ok := combos.Find(lookupModel); ok {
		for _, member := range cmb.Models {
			if trimmed := strings.ToLower(strings.TrimSpace(member.Provider)); trimmed != "" {
				candidates = append(candidates, trimmed)
			}
		}
	}
	if len(candidates) > 0 {
		return candidates, true
	}
	return nil, false
}

// extractRequestedModel reads the OpenAI-style JSON body "model" field,
// falling back to Gemini-style /models/{model}:action path segments. The
// request body is restored so downstream handlers can read it normally.
func extractRequestedModel(c *gin.Context) string {
	var model string
	if c.Request.Body != nil {
		raw, err := io.ReadAll(c.Request.Body)
		if err == nil {
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			model = strings.TrimSpace(gjson.GetBytes(raw, "model").String())
		} else {
			c.Request.Body = io.NopCloser(bytes.NewReader(nil))
		}
	}
	if model != "" {
		return model
	}
	const marker = "/models/"
	path := c.Request.URL.Path
	if idx := strings.Index(path, marker); idx >= 0 {
		rest := path[idx+len(marker):]
		if cut := strings.IndexAny(rest, ":/?"); cut >= 0 {
			rest = rest[:cut]
		}
		return strings.TrimSpace(rest)
	}
	return ""
}

// APIKeyPolicyMiddleware enforces per-key policies after AuthMiddleware has
// resolved the client key into the gin context.
func APIKeyPolicyMiddleware() gin.HandlerFunc {
	enforcer := apikeypolicy.Default()
	return func(c *gin.Context) {
		key := c.GetString("userApiKey")
		if strings.TrimSpace(key) == "" {
			c.Next()
			return
		}
		if _, hasPolicy := enforcer.Policy(key); !hasPolicy {
			c.Next()
			return
		}
		model := extractRequestedModel(c)
		if !enforcer.CheckModel(key, model) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "model_not_allowed"})
			return
		}
		if pinned := enforcer.PinnedProviderForModel(key, model); pinned != "" {
			if c.Request != nil && c.Request.Header.Get("X-Provider") == "" {
				c.Request.Header.Set("X-Provider", pinned)
			}
		}
		candidates, known := modelKnownToRegistry(model)
		if !enforcer.CheckProviders(key, candidates, known) {
			// Unknown models cannot be attributed to a provider; they pass the
			// provider check only when no provider restriction is configured
			// (CheckProviders already handles that case).
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "provider_not_allowed"})
			return
		}
		if !enforcer.CheckRateLimit(key) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate_limit_exceeded"})
			return
		}
		if !enforcer.CheckBudget(key) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "token_budget_exceeded"})
			return
		}
		c.Next()
	}
}
