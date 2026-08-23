package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// api-key-policies: []config.APIKeyPolicy
func (h *Handler) GetAPIKeyPolicies(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	masked := make([]gin.H, 0, len(h.cfg.APIKeyPolicies))
	for _, p := range h.cfg.APIKeyPolicies {
		entry := gin.H{"key": apikeypolicy.MaskedKey(p.Key)}
		if len(p.Models) > 0 {
			entry["models"] = p.Models
		}
		if len(p.Providers) > 0 {
			entry["providers"] = p.Providers
		}
		if p.Limit != nil {
			entry["token-limit"] = p.Limit
		}
		masked = append(masked, entry)
	}
	c.JSON(http.StatusOK, gin.H{"api-key-policies": masked})
}

func (h *Handler) PutAPIKeyPolicies(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	var policies []config.APIKeyPolicy
	if err = json.Unmarshal(data, &policies); err != nil {
		var obj struct {
			Items []config.APIKeyPolicy `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		policies = obj.Items
	}
	normalized, errNorm := config.NormalizeAPIKeyPolicies(policies)
	if errNorm != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errNorm.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.APIKeyPolicies = normalized
	apikeypolicy.Default().Replace(normalized)
	if !h.persistLocked(c) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "count": len(normalized)})
}

// GetAPIKeyPolicyUsage exposes in-memory per-key token counters.
func (h *Handler) GetAPIKeyPolicyUsage(c *gin.Context) {
	usage := apikeypolicy.Default().UsageSnapshot()
	if usage == nil {
		usage = []apikeypolicy.WindowUsage{}
	}
	c.JSON(http.StatusOK, gin.H{"usage": usage})
}

// ImportOpenAICompatModels fetches {base_url}/models for one configured
// OpenAI-compatible provider and merges discovered IDs into entry.models as
// name-only entries. Existing names/aliases are skipped.
func (h *Handler) ImportOpenAICompatModels(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing provider name"})
		return
	}

	client := &http.Client{Timeout: 30 * time.Second}

	h.mu.Lock()
	targetIndex := -1
	for i := range h.cfg.OpenAICompatibility {
		if h.cfg.OpenAICompatibility[i].Name == name {
			targetIndex = i
			break
		}
	}
	if targetIndex == -1 {
		h.mu.Unlock()
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	entry := h.cfg.OpenAICompatibility[targetIndex] // struct copy; models deep-copied below
	baseURL := strings.TrimRight(entry.BaseURL, "/")
	apiKey := ""
	if len(entry.APIKeyEntries) > 0 {
		apiKey = entry.APIKeyEntries[0].APIKey
	}
	h.mu.Unlock()

	if baseURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider has no base-url configured"})
		return
	}

	req, errReq := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, baseURL+"/models", nil)
	if errReq != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid base-url: %v", errReq)})
		return
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to reach upstream: %v", errDo), "added": []string{}, "skipped": []string{}})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil || resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   fmt.Sprintf("upstream returned status %d", resp.StatusCode),
			"added":   []string{},
			"skipped": []string{},
		})
		return
	}

	added, skipped := mergeDiscoveredModels(entry.Models, body)

	h.mu.Lock()
	defer h.mu.Unlock()
	current := h.cfg.OpenAICompatibility[targetIndex]
	addedCount := len(added)
	current.Models = append(append([]config.OpenAICompatibilityModel(nil), current.Models...), added...)
	h.cfg.OpenAICompatibility[targetIndex] = current
	h.cfg.SanitizeOpenAICompatibility()
	// Persist without the standard persistLocked response so this endpoint can
	// return its own merge summary.
	if errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); errSave != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", errSave)})
		return
	}
	snapshot := h.reloadSnapshotConfigLocked()
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
	c.JSON(http.StatusOK, gin.H{
		"status":      "ok",
		"added":       modelNames(added),
		"skipped":     skipped,
		"added_count": addedCount,
	})
}

func modelNames(models []config.OpenAICompatibilityModel) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Name)
	}
	return out
}

// mergeDiscoveredModels parses OpenAI-style {"data":[{"id":...}]} payloads
// (tolerating {"models":[...]} shapes) and returns new name-only entries plus
// the names skipped because they already exist as a name or alias.
func mergeDiscoveredModels(existing []config.OpenAICompatibilityModel, payload []byte) ([]config.OpenAICompatibilityModel, []string) {
	known := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		if trimmed := strings.TrimSpace(m.Name); trimmed != "" {
			known[strings.ToLower(trimmed)] = struct{}{}
		}
		if trimmed := strings.TrimSpace(m.Alias); trimmed != "" {
			known[strings.ToLower(trimmed)] = struct{}{}
		}
	}

	discovered := parseDiscoveredModelIDs(payload)
	var added []config.OpenAICompatibilityModel
	var skipped []string
	seenNew := map[string]struct{}{}
	for _, id := range discovered {
		key := strings.ToLower(id)
		if _, exists := known[key]; exists {
			skipped = append(skipped, id)
			continue
		}
		if _, exists := seenNew[key]; exists {
			skipped = append(skipped, id)
			continue
		}
		seenNew[key] = struct{}{}
		added = append(added, config.OpenAICompatibilityModel{Name: id})
	}
	return added, skipped
}

func parseDiscoveredModelIDs(payload []byte) []string {
	var root struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil
	}
	ids := make([]string, 0, len(root.Data)+len(root.Models))
	for _, item := range root.Data {
		if trimmed := strings.TrimSpace(item.ID); trimmed != "" {
			ids = append(ids, trimmed)
		}
	}
	for _, item := range root.Models {
		value := strings.TrimSpace(item.ID)
		if value == "" {
			value = strings.TrimSpace(item.Name)
		}
		if value != "" {
			ids = append(ids, value)
		}
	}
	return ids
}
