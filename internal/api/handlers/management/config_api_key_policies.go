package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
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
		if h.cfg.OpenAICompatibility[i].Name == name ||
			(name == "cloudflare-ai" && (strings.HasPrefix(h.cfg.OpenAICompatibility[i].Name, "cloudflare") || strings.Contains(h.cfg.OpenAICompatibility[i].BaseURL, "cloudflare.com"))) {
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

	listURL := baseURL + "/models"
	if strings.Contains(baseURL, "api.cloudflare.com") {
		listURL = strings.Replace(baseURL, "/ai/v1", "/ai/models/search", 1)
	}
	req, errReq := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, listURL, nil)
	if errReq != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid base-url: %v", errReq)})
		return
	}
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

	// free_only restricts imports to the provider's officially free tier,
	// so a keyless/free listing never pulls in paid models by accident.
	freeOnly := c.Query("free_only") == "true"
	prune := c.Query("prune") != "false"
	finalModels, added, pruned, skipped := syncDiscoveredModels(name, entry.Models, body, freeOnly, prune)
	h.mu.Lock()
	defer h.mu.Unlock()
	current := h.cfg.OpenAICompatibility[targetIndex]
	current.Models = finalModels
	h.cfg.OpenAICompatibility[targetIndex] = current

	if name == "cloudflare-ai" {
		for i := range h.cfg.OpenAICompatibility {
			if strings.HasPrefix(h.cfg.OpenAICompatibility[i].Name, "cloudflare") || strings.Contains(h.cfg.OpenAICompatibility[i].BaseURL, "cloudflare.com") {
				h.cfg.OpenAICompatibility[i].Models = finalModels
			}
		}
	}
	h.cfg.SanitizeOpenAICompatibility()
	// return its own merge summary.
	if errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); errSave != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", errSave)})
		return
	}
	snapshot := h.reloadSnapshotConfigLocked()
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
	c.JSON(http.StatusOK, gin.H{
		"status":       "ok",
		"added":        added,
		"skipped":      skipped,
		"pruned":       pruned,
		"added_count":  len(added),
		"pruned_count": len(pruned),
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
// isFreeModelID reports whether an OpenCode Zen model id belongs to the
// officially free tier (https://opencode.ai/docs/zen/): documented limited-
// time free listings carry a "-free" suffix, with the stealth model
// "big-pickle" as the single known exception.
func isFreeModelID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasSuffix(id, "-free") || id == "big-pickle"
}
func isDiscoveredImageModel(id string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))
	return lower == "ig" || lower == "100%" || strings.Contains(lower, "image-gen") || strings.Contains(lower, "image_gen") || strings.Contains(lower, "dall-e") || strings.Contains(lower, "flux") || strings.Contains(lower, "midjourney") || strings.Contains(lower, "imagen")
}

func isDiscoveredVisionModel(id string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))
	return strings.Contains(lower, "vision") || strings.Contains(lower, "vl") || strings.Contains(lower, "image") ||
		strings.Contains(lower, "claude") || strings.Contains(lower, "grok") || strings.Contains(lower, "step-3.7") ||
		strings.Contains(lower, "step-5") || strings.Contains(lower, "glm-5.3") || strings.Contains(lower, "deepseek-v4") ||
		strings.Contains(lower, "deepseek-unrestricted") || strings.Contains(lower, "big-pickle") || strings.Contains(lower, "mimo-v2") ||
		strings.Contains(lower, "space-bunny") || strings.Contains(lower, "qwen3.8") || lower == "jerouter" || lower == "ig" || lower == "100%"
}

type discoveredModelEntry struct {
	ID               string
	ContextLength    int
	MaxTokens        int
	InputModalities  []string
	OutputModalities []string
	IsImage          bool
}

func parseModelEntryNode(res gjson.Result) (discoveredModelEntry, bool) {
	id := strings.TrimSpace(res.Get("id").String())
	if id == "" {
		id = strings.TrimSpace(res.Get("name").String())
	}
	if id == "" {
		return discoveredModelEntry{}, false
	}

	var ctxLen int
	for _, field := range []string{"context_length", "context_window", "max_context_length", "inputTokenLimit", "max_model_len"} {
		if val := res.Get(field).Int(); val > 0 {
			ctxLen = int(val)
			break
		}
	}

	var maxTok int
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "outputTokenLimit"} {
		if val := res.Get(field).Int(); val > 0 {
			maxTok = int(val)
			break
		}
	}

	var inputMods []string
	for _, field := range []string{"architecture.input_modalities", "input_modalities", "modalities"} {
		if arr := res.Get(field); arr.Exists() && arr.IsArray() {
			for _, m := range arr.Array() {
				if s := strings.TrimSpace(m.String()); s != "" {
					inputMods = append(inputMods, strings.ToLower(s))
				}
			}
			if len(inputMods) > 0 {
				break
			}
		}
	}

	var outputMods []string
	for _, field := range []string{"architecture.output_modalities", "output_modalities"} {
		if arr := res.Get(field); arr.Exists() && arr.IsArray() {
			for _, m := range arr.Array() {
				if s := strings.TrimSpace(m.String()); s != "" {
					outputMods = append(outputMods, strings.ToLower(s))
				}
			}
			if len(outputMods) > 0 {
				break
			}
		}
	}

	isImage := isDiscoveredImageModel(id)
	if res.Get("image").Bool() || res.Get("capabilities.image").Bool() {
		isImage = true
	}

	if (res.Get("vision").Bool() || res.Get("capabilities.vision").Bool() || isDiscoveredVisionModel(id)) && len(inputMods) == 0 {
		inputMods = []string{"text", "image"}
	}

	return discoveredModelEntry{
		ID:               id,
		ContextLength:    ctxLen,
		MaxTokens:        maxTok,
		InputModalities:  inputMods,
		OutputModalities: outputMods,
		IsImage:          isImage,
	}, true
}

func parseDiscoveredModelEntries(payload []byte) []discoveredModelEntry {
	parsed := gjson.ParseBytes(payload)
	var list gjson.Result
	if parsed.IsArray() {
		list = parsed
	} else if d := parsed.Get("data"); d.IsArray() {
		list = d
	} else if m := parsed.Get("models"); m.IsArray() {
		list = m
	} else if r := parsed.Get("result"); r.IsArray() {
		list = r
	} else if it := parsed.Get("items"); it.IsArray() {
		list = it
	}

	var entries []discoveredModelEntry
	if list.IsArray() {
		for _, item := range list.Array() {
			if entry, ok := parseModelEntryNode(item); ok {
				entries = append(entries, entry)
			}
		}
		return entries
	}

	var strArr []string
	if err := json.Unmarshal(payload, &strArr); err == nil && len(strArr) > 0 {
		for _, s := range strArr {
			s = strings.TrimSpace(s)
			if s != "" {
				entries = append(entries, discoveredModelEntry{ID: s})
			}
		}
	}
	return entries
}

func parseDiscoveredModelIDs(payload []byte) []string {
	entries := parseDiscoveredModelEntries(payload)
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

func defaultCatalogContextLength(id string) int {
	lower := strings.ToLower(strings.TrimSpace(id))
	switch {
	case strings.Contains(lower, "deepseek"):
		return 1000000 // 1M
	case strings.Contains(lower, "mimo"):
		return 1000000 // 1M
	case strings.Contains(lower, "beam"):
		return 1000000 // 1M
	case strings.Contains(lower, "space-bunny"):
		return 1000000 // 1M
	case strings.Contains(lower, "longcat"):
		return 1000000 // 1M
	case strings.Contains(lower, "big-pickle"):
		return 1000000 // 1M
	case strings.Contains(lower, "grok"):
		return 1000000 // 1M
	case strings.Contains(lower, "step-5") || strings.Contains(lower, "step-3.7"):
		return 524288 // 512k
	case strings.Contains(lower, "glm-5"):
		return 524288 // 512k
	case strings.Contains(lower, "jev"):
		return 16384 // 16k
	case strings.Contains(lower, "claude"):
		return 200000 // 200k
	default:
		return 0
	}
}

func isCustomOrProtectedModel(m config.OpenAICompatibilityModel) bool {
	nameLower := strings.ToLower(strings.TrimSpace(m.Name))
	aliasLower := strings.ToLower(strings.TrimSpace(m.Alias))
	if aliasLower != "" {
		return true
	}
	for _, marker := range []string{"-jb", "_jb", "jailbreak", "unrestricted", "custom"} {
		if strings.Contains(nameLower, marker) || strings.Contains(aliasLower, marker) {
			return true
		}
	}
	return nameLower == "jb" || strings.HasPrefix(nameLower, "jb-") || strings.HasPrefix(nameLower, "jb_")
}

func syncDiscoveredModels(providerName string, existing []config.OpenAICompatibilityModel, payload []byte, freeOnly, prune bool) ([]config.OpenAICompatibilityModel, []string, []string, []string) {
	discovered := parseDiscoveredModelEntries(payload)
	validUpstream := make(map[string]discoveredModelEntry, len(discovered))
	for _, entry := range discovered {
		if freeOnly && !isFreeModelID(entry.ID) {
			continue
		}
		validUpstream[strings.ToLower(entry.ID)] = entry
	}

	var kept []config.OpenAICompatibilityModel
	var pruned []string
	known := make(map[string]struct{}, len(existing))

	for _, m := range existing {
		nameKey := strings.ToLower(strings.TrimSpace(m.Name))
		aliasKey := strings.ToLower(strings.TrimSpace(m.Alias))
		if upEntry, ok := validUpstream[nameKey]; ok {
			if upEntry.ContextLength > 0 {
				m.MaxContextLength = upEntry.ContextLength
			}
			if upEntry.IsImage && !m.Image {
				m.Image = true
			}
			if len(upEntry.InputModalities) > 0 && len(m.InputModalities) == 0 {
				m.InputModalities = upEntry.InputModalities
			}
			if len(upEntry.OutputModalities) > 0 && len(m.OutputModalities) == 0 {
				m.OutputModalities = upEntry.OutputModalities
			}
		}
		if m.MaxContextLength <= 0 {
			m.MaxContextLength = defaultCatalogContextLength(m.Name)
			if m.MaxContextLength <= 0 && aliasKey != "" {
				m.MaxContextLength = defaultCatalogContextLength(m.Alias)
			}
		}
		if isDiscoveredImageModel(m.Name) && !m.Image {
			m.Image = true
		}
		if isDiscoveredVisionModel(m.Name) && len(m.InputModalities) == 0 {
			m.InputModalities = []string{"text", "image"}
		}

		isCustom := isCustomOrProtectedModel(m)
		_, inUpstream := validUpstream[nameKey]
		_, aliasInUpstream := validUpstream[aliasKey]

		if prune {
			if inUpstream || aliasInUpstream || isCustom {
				kept = append(kept, m)
				if nameKey != "" {
					known[nameKey] = struct{}{}
				}
				if aliasKey != "" {
					known[aliasKey] = struct{}{}
				}
			} else {
				pruned = append(pruned, m.Name)
			}
		} else {
			kept = append(kept, m)
			if nameKey != "" {
				known[nameKey] = struct{}{}
			}
			if aliasKey != "" {
				known[aliasKey] = struct{}{}
			}
		}
	}

	var added []config.OpenAICompatibilityModel
	var addedNames []string
	var skipped []string
	seenNew := map[string]struct{}{}
	for _, entry := range discovered {
		if freeOnly && !isFreeModelID(entry.ID) {
			continue
		}
		key := strings.ToLower(entry.ID)
		if _, exists := known[key]; exists {
			skipped = append(skipped, entry.ID)
			continue
		}
		if _, exists := seenNew[key]; exists {
			skipped = append(skipped, entry.ID)
			continue
		}
		seenNew[key] = struct{}{}
		ctxLen := entry.ContextLength
		if ctxLen <= 0 {
			ctxLen = defaultCatalogContextLength(entry.ID)
		}
		newModel := config.OpenAICompatibilityModel{
			Name:             entry.ID,
			MaxContextLength: ctxLen,
			Image:            entry.IsImage,
			InputModalities:  entry.InputModalities,
			OutputModalities: entry.OutputModalities,
		}
		if isDiscoveredImageModel(entry.ID) {
			newModel.Image = true
		}
		if (isDiscoveredVisionModel(entry.ID) || isDiscoveredVisionModel(newModel.Name)) && len(newModel.InputModalities) == 0 {
			newModel.InputModalities = []string{"text", "image"}
		}
		added = append(added, newModel)
		addedNames = append(addedNames, entry.ID)
	}
	if strings.Contains(strings.ToLower(providerName), "-jb") || strings.Contains(strings.ToLower(providerName), "jailbreak") {
		added = nil
		addedNames = nil
	}
	finalList := append(kept, added...)
	return finalList, addedNames, pruned, skipped
}

func mergeDiscoveredModels(existing []config.OpenAICompatibilityModel, payload []byte, freeOnly bool) ([]config.OpenAICompatibilityModel, []string) {
	finalList, addedNames, _, skipped := syncDiscoveredModels("", existing, payload, freeOnly, false)
	_ = finalList
	var added []config.OpenAICompatibilityModel
	discoveredMap := make(map[string]discoveredModelEntry)
	for _, e := range parseDiscoveredModelEntries(payload) {
		discoveredMap[strings.ToLower(e.ID)] = e
	}
	for _, name := range addedNames {
		e := discoveredMap[strings.ToLower(name)]
		newModel := config.OpenAICompatibilityModel{
			Name:             name,
			MaxContextLength: e.ContextLength,
			Image:            e.IsImage,
			InputModalities:  e.InputModalities,
			OutputModalities: e.OutputModalities,
		}
		if isDiscoveredImageModel(name) {
			newModel.Image = true
		}
		if (isDiscoveredVisionModel(name) || isDiscoveredVisionModel(newModel.Name)) && len(newModel.InputModalities) == 0 {
			newModel.InputModalities = []string{"text", "image"}
		}
		added = append(added, newModel)
	}
	return added, skipped
}
