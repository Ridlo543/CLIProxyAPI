package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
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
		if !strings.Contains(raw, "://") && strings.Contains(raw, ":") {
			return "http://" + normalizeProxyEndpoint(raw)
		}
		return "invalid"
	}
	if parsed.Scheme == "" {
		return "http://" + parsed.Host
	}
	return parsed.Scheme + "://" + parsed.Host
}

func normalizeProxyEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "socks5://")
	raw = strings.TrimPrefix(raw, "socks5h://")
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		raw = raw[at+1:]
	}
	return strings.TrimRight(raw, "/")
}

type ProxyPoolResponseItem struct {
	Name           string   `json:"name"`
	Strategy       string   `json:"strategy"`
	Strict         bool     `json:"strict"`
	Cooldown       string   `json:"cooldown,omitempty"`
	EntryCount     int      `json:"entry_count"`
	Healthy        int      `json:"healthy"`
	Cooling        int      `json:"cooling"`
	FailureCount   uint64   `json:"failure_count"`
	URLs           []string `json:"urls,omitempty"`
	BoundAccounts  []string `json:"bound_accounts,omitempty"`
	BoundProviders []string `json:"bound_providers,omitempty"`
}

func (h *Handler) collectBoundAccounts(poolName string) []string {
	var accounts []string
	if poolName == "" {
		return accounts
	}
	seen := make(map[string]bool)
	add := func(acc string) {
		acc = strings.TrimSpace(acc)
		if acc != "" && !seen[acc] {
			seen[acc] = true
			accounts = append(accounts, acc)
		}
	}
	if h != nil && h.authManager != nil {
		for _, a := range h.authManager.List() {
			if a == nil {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(a.ProxyPool), poolName) {
				name := a.Label
				if name == "" {
					name = a.FileName
				}
				if name == "" {
					name = a.ID
				}
				add(name)
			}
		}
	}
	if h != nil && h.cfg != nil {
		for _, k := range h.cfg.GeminiKey {
			if strings.EqualFold(strings.TrimSpace(k.ProxyPool), poolName) {
				add("gemini-api-key")
			}
		}
		for _, k := range h.cfg.ClaudeKey {
			if strings.EqualFold(strings.TrimSpace(k.ProxyPool), poolName) {
				add("claude-api-key")
			}
		}
		for _, k := range h.cfg.CodexKey {
			if strings.EqualFold(strings.TrimSpace(k.ProxyPool), poolName) {
				add("codex-api-key")
			}
		}
		for _, compat := range h.cfg.OpenAICompatibility {
			for _, entry := range compat.APIKeyEntries {
				if strings.EqualFold(strings.TrimSpace(entry.ProxyPool), poolName) {
					add("openai-compat:" + compat.Name)
				}
			}
		}
	}
	return accounts
}

func (h *Handler) collectBoundProviders(poolName string) []string {
	var providers []string
	if poolName == "" {
		return providers
	}
	seen := make(map[string]bool)
	add := func(p string) {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" && !seen[p] {
			seen[p] = true
			providers = append(providers, p)
		}
	}
	if h != nil && h.authManager != nil {
		for _, a := range h.authManager.List() {
			if a != nil && strings.EqualFold(strings.TrimSpace(a.ProxyPool), poolName) {
				add(a.Provider)
			}
		}
	}
	if h != nil && h.cfg != nil {
		for _, k := range h.cfg.GeminiKey {
			if strings.EqualFold(strings.TrimSpace(k.ProxyPool), poolName) {
				add("gemini")
			}
		}
		for _, k := range h.cfg.ClaudeKey {
			if strings.EqualFold(strings.TrimSpace(k.ProxyPool), poolName) {
				add("claude")
			}
		}
		for _, k := range h.cfg.CodexKey {
			if strings.EqualFold(strings.TrimSpace(k.ProxyPool), poolName) {
				add("codex")
			}
		}
		for _, k := range h.cfg.XAIKey {
			if strings.EqualFold(strings.TrimSpace(k.ProxyPool), poolName) {
				add("xai")
			}
		}
		for _, compat := range h.cfg.OpenAICompatibility {
			for _, entry := range compat.APIKeyEntries {
				if strings.EqualFold(strings.TrimSpace(entry.ProxyPool), poolName) {
					add(compat.Name)
				}
			}
		}
	}
	sort.Strings(providers)
	return providers
}

// GetProxyPools returns the sanitized status of all configured proxy pools.
func (h *Handler) GetProxyPools(c *gin.Context) {
	var pools []coreauth.ProxyPoolStatus
	if h != nil && h.authManager != nil {
		if reporter, ok := h.authManager.RoundTripperProvider().(proxyPoolReporter); ok {
			pools = reporter.ProxyPoolStatuses()
		}
	}

	statusMap := make(map[string]coreauth.ProxyPoolStatus)
	for _, p := range pools {
		statusMap[strings.ToLower(strings.TrimSpace(p.Name))] = p
	}

	var results []ProxyPoolResponseItem
	seenPoolNames := make(map[string]bool)

	if h != nil && h.cfg != nil {
		for _, cp := range h.cfg.ProxyPools {
			name := strings.TrimSpace(cp.Name)
			key := strings.ToLower(name)
			seenPoolNames[key] = true

			var urls []string
			runtimeStatus, hasRuntime := statusMap[key]
			if hasRuntime && len(runtimeStatus.URLs) > 0 {
				for _, u := range runtimeStatus.URLs {
					urls = append(urls, sanitizeProxyURL(u))
				}
			} else {
				for _, entry := range cp.Entries {
					urls = append(urls, sanitizeProxyURL(entry.URL))
				}
			}

			entryCount := len(cp.Entries)
			healthy := len(cp.Entries)
			cooling := 0
			var failCount uint64
			if hasRuntime {
				if runtimeStatus.EntryCount > 0 {
					entryCount = runtimeStatus.EntryCount
				}
				healthy = runtimeStatus.Healthy
				cooling = runtimeStatus.Cooling
				failCount = runtimeStatus.FailureCount
			}

			results = append(results, ProxyPoolResponseItem{
				Name:          name,
				Strategy:      cp.Strategy,
				Strict:        cp.Strict,
				Cooldown:      cp.Cooldown,
				EntryCount:    entryCount,
				Healthy:       healthy,
				Cooling:       cooling,
				FailureCount:  failCount,
				URLs:          urls,
				BoundAccounts:  h.collectBoundAccounts(name),
				BoundProviders: h.collectBoundProviders(name),
			})
		}
	}

	for _, p := range pools {
		name := strings.TrimSpace(p.Name)
		key := strings.ToLower(name)
		if seenPoolNames[key] {
			continue
		}
		seenPoolNames[key] = true
		var urls []string
		for _, u := range p.URLs {
			urls = append(urls, sanitizeProxyURL(u))
		}
		results = append(results, ProxyPoolResponseItem{
			Name:          name,
			Strategy:      p.Strategy,
			Strict:        p.Strict,
			EntryCount:    p.EntryCount,
			Healthy:       p.Healthy,
			Cooling:       p.Cooling,
			FailureCount:  p.FailureCount,
			URLs:          urls,
			BoundAccounts:  h.collectBoundAccounts(name),
			BoundProviders: h.collectBoundProviders(name),
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	c.JSON(http.StatusOK, gin.H{"proxy-pools": results})
}

type ProxyPoolPayload struct {
	Name     string   `json:"name"`
	Strategy string   `json:"strategy"`
	Strict   bool     `json:"strict"`
	Cooldown string   `json:"cooldown"`
	Entries  []any    `json:"entries"`
}

func parseEntries(rawEntries []any) []config.ProxyPoolEntry {
	var entries []config.ProxyPoolEntry
	for _, item := range rawEntries {
		switch v := item.(type) {
		case string:
			u := strings.TrimSpace(v)
			if u != "" {
				entries = append(entries, config.ProxyPoolEntry{URL: u})
			}
		case map[string]any:
			if rawURL, ok := v["url"].(string); ok {
				u := strings.TrimSpace(rawURL)
				if u != "" {
					entries = append(entries, config.ProxyPoolEntry{URL: u})
				}
			}
		}
	}
	return entries
}

// PostProxyPool creates a new proxy pool configuration.
func (h *Handler) PostProxyPool(c *gin.Context) {
	if h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "configuration unavailable"})
		return
	}
	var req ProxyPoolPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if h.cfg.HasProxyPool(name) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("proxy pool %q already exists", name)})
		return
	}

	strategy := strings.ToLower(strings.TrimSpace(req.Strategy))
	if strategy == "" {
		strategy = "round-robin"
	}
	cooldown := strings.TrimSpace(req.Cooldown)
	if cooldown == "" {
		cooldown = "30s"
	}

	entries := parseEntries(req.Entries)
	if len(entries) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one proxy entry is required"})
		return
	}

	newPool := config.ProxyPool{
		Name:     name,
		Strategy: strategy,
		Strict:   req.Strict,
		Cooldown: cooldown,
		Entries:  entries,
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.cfg.ProxyPools = append(h.cfg.ProxyPools, newPool)
	if err := h.cfg.NormalizeProxyPools(); err != nil {
		h.cfg.ProxyPools = h.cfg.ProxyPools[:len(h.cfg.ProxyPools)-1]
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	h.persistProxyPools(c, http.StatusCreated, gin.H{"status": "ok", "pool": newPool})
}

// PutProxyPool updates an existing proxy pool.
func (h *Handler) PutProxyPool(c *gin.Context) {
	if h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "configuration unavailable"})
		return
	}
	targetName := strings.TrimSpace(c.Param("name"))
	if targetName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pool name parameter is required"})
		return
	}

	var req ProxyPoolPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	targetIdx := -1
	for i := range h.cfg.ProxyPools {
		if strings.EqualFold(strings.TrimSpace(h.cfg.ProxyPools[i].Name), targetName) {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("proxy pool %q not found", targetName)})
		return
	}

	existing := h.cfg.ProxyPools[targetIdx]
	newName := strings.TrimSpace(req.Name)
	if newName == "" {
		newName = existing.Name
	}

	strategy := strings.ToLower(strings.TrimSpace(req.Strategy))
	if strategy == "" {
		strategy = existing.Strategy
	}
	cooldown := strings.TrimSpace(req.Cooldown)
	if cooldown == "" {
		cooldown = existing.Cooldown
	}

	entries := parseEntries(req.Entries)
	if len(entries) == 0 {
		entries = existing.Entries
	} else {
		// Preserve existing passwords if entries were sent as sanitized host:port
		for j := range entries {
			for _, oldEntry := range existing.Entries {
				if sanitizeProxyURL(oldEntry.URL) == entries[j].URL && !strings.Contains(entries[j].URL, "@") {
					entries[j].URL = oldEntry.URL
					break
				}
			}
		}
	}

	h.cfg.ProxyPools[targetIdx] = config.ProxyPool{
		Name:     newName,
		Strategy: strategy,
		Strict:   req.Strict,
		Cooldown: cooldown,
		Entries:  entries,
	}

	// Update references if pool was renamed
	if !strings.EqualFold(existing.Name, newName) {
		h.updateProxyPoolReferences(existing.Name, newName)
	}

	if err := h.cfg.NormalizeProxyPools(); err != nil {
		h.cfg.ProxyPools[targetIdx] = existing
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	h.persistProxyPools(c, http.StatusOK, gin.H{"status": "ok", "pool": h.cfg.ProxyPools[targetIdx]})
}

// DeleteProxyPool deletes a proxy pool and clears references to it.
func (h *Handler) DeleteProxyPool(c *gin.Context) {
	if h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "configuration unavailable"})
		return
	}
	targetName := strings.TrimSpace(c.Param("name"))
	if targetName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pool name parameter is required"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	targetIdx := -1
	for i := range h.cfg.ProxyPools {
		if strings.EqualFold(strings.TrimSpace(h.cfg.ProxyPools[i].Name), targetName) {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("proxy pool %q not found", targetName)})
		return
	}

	// Clear references across config and auth files
	h.updateProxyPoolReferences(targetName, "")

	h.cfg.ProxyPools = append(h.cfg.ProxyPools[:targetIdx], h.cfg.ProxyPools[targetIdx+1:]...)
	if err := h.cfg.NormalizeProxyPools(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	h.persistProxyPools(c, http.StatusOK, gin.H{"status": "ok", "deleted": targetName})
}

func (h *Handler) updateProxyPoolReferences(oldPoolName, newPoolName string) {
	if h == nil || h.cfg == nil {
		return
	}
	for i := range h.cfg.GeminiKey {
		if strings.EqualFold(strings.TrimSpace(h.cfg.GeminiKey[i].ProxyPool), oldPoolName) {
			h.cfg.GeminiKey[i].ProxyPool = newPoolName
		}
	}
	for i := range h.cfg.InteractionsKey {
		if strings.EqualFold(strings.TrimSpace(h.cfg.InteractionsKey[i].ProxyPool), oldPoolName) {
			h.cfg.InteractionsKey[i].ProxyPool = newPoolName
		}
	}
	for i := range h.cfg.ClaudeKey {
		if strings.EqualFold(strings.TrimSpace(h.cfg.ClaudeKey[i].ProxyPool), oldPoolName) {
			h.cfg.ClaudeKey[i].ProxyPool = newPoolName
		}
	}
	for i := range h.cfg.CodexKey {
		if strings.EqualFold(strings.TrimSpace(h.cfg.CodexKey[i].ProxyPool), oldPoolName) {
			h.cfg.CodexKey[i].ProxyPool = newPoolName
		}
	}
	for i := range h.cfg.XAIKey {
		if strings.EqualFold(strings.TrimSpace(h.cfg.XAIKey[i].ProxyPool), oldPoolName) {
			h.cfg.XAIKey[i].ProxyPool = newPoolName
		}
	}
	for i := range h.cfg.VertexCompatAPIKey {
		if strings.EqualFold(strings.TrimSpace(h.cfg.VertexCompatAPIKey[i].ProxyPool), oldPoolName) {
			h.cfg.VertexCompatAPIKey[i].ProxyPool = newPoolName
		}
	}
	for i := range h.cfg.OpenAICompatibility {
		for j := range h.cfg.OpenAICompatibility[i].APIKeyEntries {
			if strings.EqualFold(strings.TrimSpace(h.cfg.OpenAICompatibility[i].APIKeyEntries[j].ProxyPool), oldPoolName) {
				h.cfg.OpenAICompatibility[i].APIKeyEntries[j].ProxyPool = newPoolName
			}
		}
	}
	if h.authManager != nil {
		for _, a := range h.authManager.List() {
			if a != nil && strings.EqualFold(strings.TrimSpace(a.ProxyPool), oldPoolName) {
				a.ProxyPool = newPoolName
				if a.Metadata != nil {
					if newPoolName == "" {
						delete(a.Metadata, "proxy_pool")
					} else {
						a.Metadata["proxy_pool"] = newPoolName
					}
				}
				h.persistAuthFileProxyPool(a, newPoolName)
			}
		}
	}
}

func (h *Handler) persistAuthFileProxyPool(a *coreauth.Auth, poolName string) {
	if h == nil || h.cfg == nil || a == nil {
		return
	}
	fileName := a.FileName
	if fileName == "" {
		fileName = a.ID
	}
	if fileName == "" {
		return
	}
	filePath := filepath.Join(h.cfg.AuthDir, fileName)
	if a.Attributes != nil && a.Attributes["path"] != "" {
		filePath = a.Attributes["path"]
	}
	data, errRead := os.ReadFile(filePath)
	if errRead != nil {
		if !strings.HasSuffix(filePath, ".json") {
			filePath += ".json"
			data, errRead = os.ReadFile(filePath)
		}
		if errRead != nil {
			return
		}
	}
	var rawMap map[string]any
	if errUnmarshal := json.Unmarshal(data, &rawMap); errUnmarshal != nil {
		return
	}
	if poolName == "" {
		delete(rawMap, "proxy_pool")
	} else {
		rawMap["proxy_pool"] = poolName
	}
	if outBytes, errMarshal := json.MarshalIndent(rawMap, "", "  "); errMarshal == nil {
		_ = os.WriteFile(filePath, outBytes, 0o600)
	}
}

// VerifyProxy tests connectivity through a specified proxy URL.
func (h *Handler) VerifyProxy(c *gin.Context) {
	var req struct {
		URL  string `json:"url"`
		Pool string `json:"pool"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	rawURL := strings.TrimSpace(req.URL)
	poolName := strings.TrimSpace(req.Pool)

	targetEndpoint := normalizeProxyEndpoint(rawURL)
	if h.cfg != nil {
		for _, p := range h.cfg.ProxyPools {
			if poolName == "" || strings.EqualFold(strings.TrimSpace(p.Name), poolName) {
				for _, entry := range p.Entries {
					if normalizeProxyEndpoint(entry.URL) == targetEndpoint || rawURL == "" {
						rawURL = entry.URL
						break
					}
				}
			}
		}
	}

	if rawURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}

	start := time.Now()
	transport, _, errTransport := proxyutil.BuildHTTPTransport(rawURL)
	if errTransport != nil {
		c.JSON(http.StatusOK, gin.H{
			"ok":         false,
			"error":      fmt.Sprintf("invalid proxy: %v", errTransport),
			"endpoint":   sanitizeProxyURL(rawURL),
			"latency_ms": 0,
		})
		return
	}
	if transport == nil {
		transport = proxyutil.NewDirectTransport()
	}
	transport.TLSHandshakeTimeout = 6 * time.Second
	transport.ResponseHeaderTimeout = 6 * time.Second

	client := &http.Client{
		Transport: transport,
		Timeout:   8 * time.Second,
	}

	reqPing, errReq := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, "http://connectivitycheck.gstatic.com/generate_204", nil)
	if errReq != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": errReq.Error()})
		return
	}
	reqPing.Header.Set("User-Agent", "curl/7.88.1")

	resp, errDo := client.Do(reqPing)
	latency := time.Since(start).Milliseconds()
	if errDo != nil {
		c.JSON(http.StatusOK, gin.H{
			"ok":         false,
			"error":      errDo.Error(),
			"endpoint":   sanitizeProxyURL(rawURL),
			"latency_ms": latency,
		})
		return
	}
	_ = resp.Body.Close()

	c.JSON(http.StatusOK, gin.H{
		"ok":          resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code": resp.StatusCode,
		"endpoint":    sanitizeProxyURL(rawURL),
		"latency_ms":  latency,
	})
}

// AssignProxyPool binds or unbinds proxy pools for accounts or providers.
func (h *Handler) AssignProxyPool(c *gin.Context) {
	var req struct {
		Pool     string   `json:"pool"` // empty, "direct", or named pool
		Provider string   `json:"provider"`
		Accounts []string `json:"accounts"`
		Scope    string   `json:"scope"` // "all", "provider", "accounts"
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	targetPool := strings.TrimSpace(req.Pool)
	if strings.EqualFold(targetPool, "direct") || strings.EqualFold(targetPool, "none") {
		targetPool = ""
	}
	if targetPool != "" && !h.cfg.HasProxyPool(targetPool) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("proxy pool %q does not exist", targetPool)})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	updatedCount := 0
	targetProvider := strings.ToLower(strings.TrimSpace(req.Provider))
	targetAccounts := make(map[string]bool)
	for _, a := range req.Accounts {
		targetAccounts[strings.TrimSpace(a)] = true
	}

	if h.authManager != nil {
		for _, a := range h.authManager.List() {
			if a == nil {
				continue
			}
			match := false
			if req.Scope == "all" {
				match = true
			} else if targetProvider != "" && strings.EqualFold(a.Provider, targetProvider) {
				match = true
			} else if targetAccounts[a.ID] || targetAccounts[a.FileName] || targetAccounts[a.Label] {
				match = true
			}

			if match {
				a.ProxyPool = targetPool
				if a.Metadata == nil {
					a.Metadata = make(map[string]any)
				}
				if targetPool == "" {
					delete(a.Metadata, "proxy_pool")
				} else {
					a.Metadata["proxy_pool"] = targetPool
				}
				a.UpdatedAt = time.Now()
				_, _ = h.authManager.Update(context.Background(), a)
				h.persistAuthFileProxyPool(a, targetPool)
				updatedCount++
			}
		}
	}

	// Also update provider-level config keys if provider was targeted or scope == all
	if req.Scope == "all" || targetProvider != "" {
		if req.Scope == "all" || targetProvider == "gemini" {
			for i := range h.cfg.GeminiKey {
				h.cfg.GeminiKey[i].ProxyPool = targetPool
			}
		}
		if req.Scope == "all" || targetProvider == "claude" {
			for i := range h.cfg.ClaudeKey {
				h.cfg.ClaudeKey[i].ProxyPool = targetPool
			}
		}
		if req.Scope == "all" || targetProvider == "codex" {
			for i := range h.cfg.CodexKey {
				h.cfg.CodexKey[i].ProxyPool = targetPool
			}
		}
		if req.Scope == "all" || targetProvider == "xai" {
			for i := range h.cfg.XAIKey {
				h.cfg.XAIKey[i].ProxyPool = targetPool
			}
		}
		for i := range h.cfg.OpenAICompatibility {
			if req.Scope == "all" || strings.EqualFold(h.cfg.OpenAICompatibility[i].Name, targetProvider) {
				for j := range h.cfg.OpenAICompatibility[i].APIKeyEntries {
					h.cfg.OpenAICompatibility[i].APIKeyEntries[j].ProxyPool = targetPool
				}
			}
		}
		_ = h.cfg.NormalizeProxyPools()
		if h.configFilePath != "" {
			_ = config.SaveConfigPreserveComments(h.configFilePath, h.cfg, c.GetBool(ConfigV8ContextKey))
			snapshot := h.reloadSnapshotConfigLocked()
			var reqCtx context.Context
			if c != nil && c.Request != nil {
				reqCtx = c.Request.Context()
			}
			h.reloadConfigAfterManagementSaveAsync(reqCtx, snapshot)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":        "ok",
		"pool":          targetPool,
		"updated_count": updatedCount,
	})
}

func (h *Handler) persistProxyPools(c *gin.Context, status int, payload any) bool {
	if h.configFilePath != "" {
		if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg, c.GetBool(ConfigV8ContextKey)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
			return false
		}
		snapshot := h.reloadSnapshotConfigLocked()
		var reqCtx context.Context
		if c != nil && c.Request != nil {
			reqCtx = c.Request.Context()
		}
		h.reloadConfigAfterManagementSaveAsync(reqCtx, snapshot)
	}
	c.JSON(status, payload)
	return true
}
