package management

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type probeProviderRequest struct {
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// ProbeProvider verifies a provider connection from the ROUTER HOST so the
// control panel never fights browser CORS. It performs a cheap authenticated
// GET of the provider's model listing:
//
//	openai-compat → GET {base-url}/models            (Authorization: Bearer)
//	anthropic     → GET {base-url}/v1/models         (x-api-key + version)
//
// POST /v0/management/provider-probe  body: {"kind","base_url","api_key"}
//
// The response is always HTTP 200 with an {"ok": bool, ...} envelope so the
// UI can render a single human-readable verdict; only malformed requests get
// a 400. Read-only: nothing is persisted.
func (h *Handler) ProbeProvider(c *gin.Context) {
	var req probeProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid request body"})
		return
	}

	baseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	apiKey := strings.TrimSpace(req.APIKey)
	kind := strings.ToLower(strings.TrimSpace(req.Kind))

	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "base_url must start with http:// or https://"})
		return
	}
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "api_key is required"})
		return
	}

	listURL := baseURL + "/models"
	headers := map[string]string{
		"Accept": "application/json",
	}
	switch kind {
	case "anthropic":
		if !strings.HasSuffix(baseURL, "/v1") {
			listURL = baseURL + "/v1/models"
		}
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	default: // "openai-compat"
		headers["Authorization"] = "Bearer " + apiKey
	}

	httpReq, errReq := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, listURL, nil)
	if errReq != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": fmt.Sprintf("Invalid base URL: %v", errReq)})
		return
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": "Could not reach the provider host — check the Base URL."})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusOK, gin.H{
			"ok":     false,
			"status": resp.StatusCode,
			"error":  humanizeProbeStatus(resp.StatusCode),
		})
		return
	}

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": "Reached the provider but could not read its response."})
		return
	}

	modelCount := len(parseDiscoveredModelIDs(body))
	c.JSON(http.StatusOK, gin.H{"ok": true, "status": http.StatusOK, "model_count": modelCount})
}

func humanizeProbeStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "The API key was rejected (401). Double-check it."
	case http.StatusForbidden:
		return "This key is valid but not allowed to list models (403)."
	case http.StatusNotFound:
		return "No model listing found at that Base URL (404) — check the path."
	case http.StatusTooManyRequests:
		return "The provider rate-limited the check (429) — try again shortly."
	default:
		return fmt.Sprintf("The provider answered with HTTP %d instead of a model listing.", status)
	}
}
