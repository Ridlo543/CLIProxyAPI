package management

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// GetOpenAICompatUpstreamGet performs a GET against a named
// openai-compatibility entry's own upstream origin using that entry's first
// API key. The requested path must stay on the entry's base-url origin, so
// this works as a generic "provider quota/info" fetch (e.g. DeepSeek's
// /user/balance) without exposing arbitrary proxying.
//
// GET /v0/management/openai-compatibility/:name/upstream-get?path=/user/balance
func (h *Handler) GetOpenAICompatUpstreamGet(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	name := strings.TrimSpace(c.Param("name"))
	rawPath := c.Query("path")
	if name == "" || !strings.HasPrefix(rawPath, "/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and absolute path are required"})
		return
	}

	var entry *struct {
		baseURL string
		apiKey  string
	}
	for i := range h.cfg.OpenAICompatibility {
		e := &h.cfg.OpenAICompatibility[i]
		if e.Name == name || strings.HasPrefix(e.Name, name) {
			entry = &struct {
				baseURL string
				apiKey  string
			}{e.BaseURL, ""}
			for _, ke := range e.APIKeyEntries {
				if strings.TrimSpace(ke.APIKey) != "" {
					entry.apiKey = ke.APIKey
					break
				}
			}
			break
		}
	}
	if entry == nil || strings.TrimSpace(entry.baseURL) == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no openai-compatibility entry matching %q", name)})
		return
	}

	base, err := url.Parse(strings.TrimRight(entry.baseURL, "/"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "entry base-url is invalid"})
		return
	}
	ref, err := url.Parse(base.Path + rawPath)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}
	target := base.ResolveReference(ref)

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("build request: %v", err)})
		return
	}
	if entry.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+entry.apiKey)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("upstream request failed: %v", err)})
		return
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Errorf("close upstream body: %v", cerr)
		}
	}()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c.Data(resp.StatusCode, "application/json", body)
}
