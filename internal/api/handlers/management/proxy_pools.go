package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type proxyPoolReporter interface {
	ProxyPoolStatuses() []coreauth.ProxyPoolStatus
	RecordEntryFailure(poolName, rawURL string)
	RecordEntrySuccess(poolName, rawURL string)
}

type proxyPoolConfigUpdater interface {
	UpdateConfig(cfg *config.Config)
}

func (h *Handler) getProxyPoolReporter() proxyPoolReporter {
	if h != nil && h.authManager != nil {
		if reporter, ok := h.authManager.RoundTripperProvider().(proxyPoolReporter); ok {
			return reporter
		}
	}
	return nil
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
			return normalizeProxyEndpointStrict(raw)
		}
		return "invalid"
	}
	if parsed.Scheme == "" {
		return normalizeProxyEndpointStrict(parsed.Host)
	}
	return parsed.Scheme + "://" + parsed.Host
}

// normalizeProxyEndpointStrict returns "http://host:port" only when the endpoint
// parses as a real host:port after dropping any credentials and path. Anything
// else is rejected instead of being echoed back with a scheme bolted on, which
// would leak the raw input (and secrets inside it) into the panel and logs.
func normalizeProxyEndpointStrict(raw string) string {
	candidate := normalizeProxyEndpoint(raw)
	if candidate == "" || strings.ContainsAny(candidate, " \t") {
		return "invalid"
	}
	parsed, err := url.Parse("http://" + candidate)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return "invalid"
	}
	if port := parsed.Port(); port != "" {
		if n, convErr := strconv.Atoi(port); convErr != nil || n <= 0 || n > 65535 {
			return "invalid"
		}
	}
	return "http://" + candidate
}
func bindFlexibleJSON(c *gin.Context, obj any) error {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return fmt.Errorf("empty request body")
	}
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return err
	}
	var wrapper map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &wrapper); errUnmarshal == nil {
		if inner, ok := wrapper["body"]; ok && len(inner) > 0 {
			var testObj map[string]any
			if errInner := json.Unmarshal(inner, &testObj); errInner == nil {
				return json.Unmarshal(inner, obj)
			}
		}
	}
	return json.Unmarshal(data, obj)
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
	CoolingURLs    []string `json:"cooling_urls,omitempty"`
	BoundAccounts  []string `json:"bound_accounts,omitempty"`
	BoundProviders []string `json:"bound_providers,omitempty"`
}

func poolContains(assigned, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return false
	}
	for _, part := range strings.FieldsFunc(assigned, func(r rune) bool {
		return r == ',' || r == '+' || r == ';'
	}) {
		if strings.EqualFold(strings.TrimSpace(part), target) {
			return true
		}
	}
	return false
}

func togglePoolInList(current, target string) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.EqualFold(target, "direct") || strings.EqualFold(target, "none") {
		return ""
	}
	var list []string
	exists := false
	for _, part := range strings.FieldsFunc(current, func(r rune) bool {
		return r == ',' || r == '+' || r == ';'
	}) {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if strings.EqualFold(p, target) {
			exists = true
		} else {
			list = append(list, p)
		}
	}
	if !exists {
		list = append(list, target)
	}
	return strings.Join(list, ",")
}

func replacePoolInList(current, oldName, newName string) string {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	var list []string
	for _, part := range strings.FieldsFunc(current, func(r rune) bool {
		return r == ',' || r == '+' || r == ';'
	}) {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if strings.EqualFold(p, oldName) {
			if newName != "" && !strings.EqualFold(newName, "direct") && !strings.EqualFold(newName, "none") {
				list = append(list, newName)
			}
		} else {
			list = append(list, p)
		}
	}
	return strings.Join(list, ",")
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
			if poolContains(a.ProxyPool, poolName) {
				name := a.Label
				if name == "" {
					name = a.FileName
				}
				if name == "" {
					name = a.ID
				}
				name = strings.TrimPrefix(name, "openai-compatibility:")
				add(name)
			}
		}
	}
	if h != nil && h.cfg != nil {
		for _, k := range h.cfg.GeminiKey {
			if poolContains(k.ProxyPool, poolName) {
				add("gemini-api-key")
			}
		}
		for _, k := range h.cfg.ClaudeKey {
			if poolContains(k.ProxyPool, poolName) {
				add("claude-api-key")
			}
		}
		for _, k := range h.cfg.CodexKey {
			if poolContains(k.ProxyPool, poolName) {
				add("codex-api-key")
			}
		}
		for _, compat := range h.cfg.OpenAICompatibility {
			for _, entry := range compat.APIKeyEntries {
				if poolContains(entry.ProxyPool, poolName) {
					add(compat.Name)
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
		p = strings.TrimPrefix(p, "openai-compatible-")
		p = strings.TrimPrefix(p, "openai-compatibility:")
		if p != "" && !seen[p] {
			seen[p] = true
			providers = append(providers, p)
		}
	}
	if h != nil && h.authManager != nil {
		for _, a := range h.authManager.List() {
			if a != nil && poolContains(a.ProxyPool, poolName) {
				add(a.Provider)
			}
		}
	}
	if h != nil && h.cfg != nil {
		for _, k := range h.cfg.GeminiKey {
			if poolContains(k.ProxyPool, poolName) {
				add("gemini")
			}
		}
		for _, k := range h.cfg.ClaudeKey {
			if poolContains(k.ProxyPool, poolName) {
				add("claude")
			}
		}
		for _, k := range h.cfg.CodexKey {
			if poolContains(k.ProxyPool, poolName) {
				add("codex")
			}
		}
		for _, k := range h.cfg.XAIKey {
			if poolContains(k.ProxyPool, poolName) {
				add("xai")
			}
		}
		for _, compat := range h.cfg.OpenAICompatibility {
			for _, entry := range compat.APIKeyEntries {
				if poolContains(entry.ProxyPool, poolName) {
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
			for _, entry := range cp.Entries {
				urls = append(urls, sanitizeProxyURL(entry.URL))
			}

			entryCount := len(cp.Entries)
			cooling := 0
			var coolingURLs []string
			runtimeStatus, hasRuntime := statusMap[key]
			if hasRuntime {
				for _, u := range urls {
					for _, cu := range runtimeStatus.CoolingURLs {
						if sanitizeProxyURL(cu) == u {
							cooling++
							coolingURLs = append(coolingURLs, u)
							break
						}
					}
				}
			}
			healthy := entryCount - cooling
			if healthy < 0 {
				healthy = 0
			}
			var failCount uint64
			if hasRuntime {
				failCount = runtimeStatus.FailureCount
			}

			results = append(results, ProxyPoolResponseItem{
				Name:           name,
				Strategy:       cp.Strategy,
				Strict:         cp.Strict,
				Cooldown:       cp.Cooldown,
				EntryCount:     entryCount,
				Healthy:        healthy,
				Cooling:        cooling,
				FailureCount:   failCount,
				URLs:           urls,
				CoolingURLs:    coolingURLs,
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
			Name:           name,
			Strategy:       p.Strategy,
			Strict:         p.Strict,
			EntryCount:     p.EntryCount,
			Healthy:        p.Healthy,
			Cooling:        p.Cooling,
			FailureCount:   p.FailureCount,
			URLs:           urls,
			CoolingURLs:    p.CoolingURLs,
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
	Name     string `json:"name"`
	Strategy string `json:"strategy"`
	Strict   bool   `json:"strict"`
	Cooldown string `json:"cooldown"`
	Entries  []any  `json:"entries"`
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
	if err := bindFlexibleJSON(c, &req); err != nil {
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
	if err := bindFlexibleJSON(c, &req); err != nil {
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
		h.cfg.GeminiKey[i].ProxyPool = replacePoolInList(h.cfg.GeminiKey[i].ProxyPool, oldPoolName, newPoolName)
	}
	for i := range h.cfg.InteractionsKey {
		h.cfg.InteractionsKey[i].ProxyPool = replacePoolInList(h.cfg.InteractionsKey[i].ProxyPool, oldPoolName, newPoolName)
	}
	for i := range h.cfg.ClaudeKey {
		h.cfg.ClaudeKey[i].ProxyPool = replacePoolInList(h.cfg.ClaudeKey[i].ProxyPool, oldPoolName, newPoolName)
	}
	for i := range h.cfg.CodexKey {
		h.cfg.CodexKey[i].ProxyPool = replacePoolInList(h.cfg.CodexKey[i].ProxyPool, oldPoolName, newPoolName)
	}
	for i := range h.cfg.XAIKey {
		h.cfg.XAIKey[i].ProxyPool = replacePoolInList(h.cfg.XAIKey[i].ProxyPool, oldPoolName, newPoolName)
	}
	for i := range h.cfg.VertexCompatAPIKey {
		h.cfg.VertexCompatAPIKey[i].ProxyPool = replacePoolInList(h.cfg.VertexCompatAPIKey[i].ProxyPool, oldPoolName, newPoolName)
	}
	for i := range h.cfg.OpenAICompatibility {
		for j := range h.cfg.OpenAICompatibility[i].APIKeyEntries {
			h.cfg.OpenAICompatibility[i].APIKeyEntries[j].ProxyPool = replacePoolInList(h.cfg.OpenAICompatibility[i].APIKeyEntries[j].ProxyPool, oldPoolName, newPoolName)
		}
	}
	if h.authManager != nil {
		for _, a := range h.authManager.List() {
			if a != nil && poolContains(a.ProxyPool, oldPoolName) {
				updated := replacePoolInList(a.ProxyPool, oldPoolName, newPoolName)
				a.ProxyPool = updated
				if a.Metadata != nil {
					if updated == "" {
						delete(a.Metadata, "proxy_pool")
					} else {
						a.Metadata["proxy_pool"] = updated
					}
				}
				a.UpdatedAt = time.Now()
				_, _ = h.authManager.Update(context.Background(), a)
				h.persistAuthFileProxyPool(a, updated)
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
	if err := bindFlexibleJSON(c, &req); err != nil {
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
	// 1. Resolve exit IP. Ask for IPv6 first: an IPv6-pinned entry still answers
	// IPv4 echo services through its IPv4 path, so an IPv4-only probe would
	// report the host address and hide the fact that the entry egresses on a
	// different family. Fall back to IPv4 for entries with no IPv6 path.
	exitIP := ""
	for _, probe := range []string{"https://ipv6.icanhazip.com", "https://ipv4.icanhazip.com"} {
		reqIP, errIP := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, probe, nil)
		if errIP != nil {
			continue
		}
		reqIP.Header.Set("User-Agent", "curl/7.88.1")
		respIP, errDoIP := client.Do(reqIP)
		if errDoIP != nil {
			continue
		}
		if b, errRead := io.ReadAll(respIP.Body); errRead == nil {
			exitIP = strings.TrimSpace(string(b))
		}
		_ = respIP.Body.Close()
		if exitIP != "" {
			break
		}
	}

	// 2. Perform AI Gateway verification (OpenCode Zen / SystemOne)
	probeURL := "https://opencode.ai/zen/v1/systemone"
	probePayload := []byte(`{"model":"jev-1.13-free","state":"ping","questions":{"q":{"type":"noul","instructions":"ping"}}}`)
	reqProbe, errProbe := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, probeURL, bytes.NewReader(probePayload))
	if errProbe == nil {
		reqProbe.Header.Set("Content-Type", "application/json")
		reqProbe.Header.Set("User-Agent", "opencode/1.18.35 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14")
		reqProbe.Header.Set("x-opencode-client", "cli")
		reqProbe.Header.Set("x-opencode-project", "global")
		reqProbe.Header.Set("x-opencode-session", "ses_test_verify_123456789012")
		reqProbe.Header.Set("x-opencode-request", "msg_test_verify_123456789012")
		reqProbe.Header.Set("Authorization", "Bearer public")

		respProbe, errDoProbe := client.Do(reqProbe)
		latency := time.Since(start).Milliseconds()
		if errDoProbe == nil {
			defer respProbe.Body.Close()
			if respProbe.StatusCode == http.StatusForbidden {
				if reporter := h.getProxyPoolReporter(); reporter != nil {
					reporter.RecordEntryFailure(poolName, rawURL)
				}
				c.JSON(http.StatusOK, gin.H{
					"ok":          false,
					"banned":      true,
					"status_code": 403,
					"error":       "Terbanned Cloudflare/OpenCode (403)",
					"endpoint":    sanitizeProxyURL(rawURL),
					"exit_ip":     exitIP,
					"is_ipv6":     strings.Contains(exitIP, ":"),
					"latency_ms":  latency,
				})
				return
			}
			if respProbe.StatusCode == http.StatusTooManyRequests {
				// 429 is an upstream AI rate-limit / capacity error, NOT a proxy transport failure.
				// Do not record failure on the live router reporter to avoid taking down healthy proxies.
				c.JSON(http.StatusOK, gin.H{
					"ok":          false,
					"banned":      false,
					"status_code": 429,
					"error":       "Upstream Capacity / Rate Limit (429)",
					"endpoint":    sanitizeProxyURL(rawURL),
					"exit_ip":     exitIP,
					"is_ipv6":     strings.Contains(exitIP, ":"),
					"latency_ms":  latency,
				})
				return
			}
			if respProbe.StatusCode >= 200 && respProbe.StatusCode < 300 {
				if reporter := h.getProxyPoolReporter(); reporter != nil {
					reporter.RecordEntrySuccess(poolName, rawURL)
				}
				c.JSON(http.StatusOK, gin.H{
					"ok":          true,
					"banned":      false,
					"status_code": respProbe.StatusCode,
					"endpoint":    sanitizeProxyURL(rawURL),
					"exit_ip":     exitIP,
					"is_ipv6":     strings.Contains(exitIP, ":"),
					"latency_ms":  latency,
				})
				return
			}
		}
	}
	// 3. Fallback to connectivity check
	reqPing, errReq := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, "http://connectivitycheck.gstatic.com/generate_204", nil)
	if errReq != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": errReq.Error()})
		return
	}
	reqPing.Header.Set("User-Agent", "curl/7.88.1")

	resp, errDo := client.Do(reqPing)
	latency := time.Since(start).Milliseconds()
	if errDo != nil {
		if reporter := h.getProxyPoolReporter(); reporter != nil {
			reporter.RecordEntryFailure(poolName, rawURL)
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":         false,
			"error":      errDo.Error(),
			"endpoint":   sanitizeProxyURL(rawURL),
			"exit_ip":    exitIP,
			"is_ipv6":    strings.Contains(exitIP, ":"),
			"latency_ms": latency,
		})
		return
	}
	_ = resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if reporter := h.getProxyPoolReporter(); reporter != nil {
			reporter.RecordEntrySuccess(poolName, rawURL)
		}
	} else {
		if reporter := h.getProxyPoolReporter(); reporter != nil {
			reporter.RecordEntryFailure(poolName, rawURL)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":          resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code": resp.StatusCode,
		"endpoint":    sanitizeProxyURL(rawURL),
		"exit_ip":     exitIP,
		"is_ipv6":     strings.Contains(exitIP, ":"),
		"latency_ms":  latency,
	})
}

// AssignProxyPool binds or unbinds proxy pools for accounts or providers.
func (h *Handler) AssignProxyPool(c *gin.Context) {
	var req struct {
		Pool     string   `json:"pool"` // empty, "direct", or named pool
		Provider string   `json:"provider"`
		Accounts []string `json:"accounts"`
		Scope    string   `json:"scope"`  // "all", "provider", "accounts"
		Action   string   `json:"action"` // "toggle" (default for single pool toggle), "set", "remove"
	}
	if err := bindFlexibleJSON(c, &req); err != nil {
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

	computeNewPool := func(current string) string {
		if targetPool == "" {
			return ""
		}
		if req.Action == "set" {
			return targetPool
		}
		if req.Action == "remove" {
			return replacePoolInList(current, targetPool, "")
		}
		if req.Action == "add" {
			if poolContains(current, targetPool) {
				return current
			}
			trimmed := strings.TrimSpace(current)
			if trimmed == "" || strings.EqualFold(trimmed, "direct") || strings.EqualFold(trimmed, "none") {
				return targetPool
			}
			return trimmed + "," + targetPool
		}
		// Default action: toggle inclusion in list
		return togglePoolInList(current, targetPool)
	}

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
			} else if targetProvider != "" && (strings.EqualFold(a.Provider, targetProvider) ||
				strings.EqualFold(strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(a.Provider), "openai-compatible-"), "openai-compatibility:"), strings.ToLower(targetProvider))) {
				match = true
			} else if targetAccounts[a.ID] || targetAccounts[a.FileName] || targetAccounts[a.Label] || targetAccounts[strings.TrimPrefix(a.Label, "openai-compatibility:")] {
				match = true
			}

			if match {
				newPool := computeNewPool(a.ProxyPool)
				a.ProxyPool = newPool
				if a.Metadata == nil {
					a.Metadata = make(map[string]any)
				}
				if newPool == "" {
					delete(a.Metadata, "proxy_pool")
				} else {
					a.Metadata["proxy_pool"] = newPool
				}
				a.UpdatedAt = time.Now()
				_, _ = h.authManager.Update(context.Background(), a)
				h.persistAuthFileProxyPool(a, newPool)
				updatedCount++
			}
		}
	}

	// Also update provider-level config keys if provider was targeted or scope == all
	if req.Scope == "all" || targetProvider != "" {
		if req.Scope == "all" || targetProvider == "gemini" {
			for i := range h.cfg.GeminiKey {
				h.cfg.GeminiKey[i].ProxyPool = computeNewPool(h.cfg.GeminiKey[i].ProxyPool)
			}
		}
		if req.Scope == "all" || targetProvider == "claude" {
			for i := range h.cfg.ClaudeKey {
				h.cfg.ClaudeKey[i].ProxyPool = computeNewPool(h.cfg.ClaudeKey[i].ProxyPool)
			}
		}
		if req.Scope == "all" || targetProvider == "codex" {
			for i := range h.cfg.CodexKey {
				h.cfg.CodexKey[i].ProxyPool = computeNewPool(h.cfg.CodexKey[i].ProxyPool)
			}
		}
		if req.Scope == "all" || targetProvider == "xai" {
			for i := range h.cfg.XAIKey {
				h.cfg.XAIKey[i].ProxyPool = computeNewPool(h.cfg.XAIKey[i].ProxyPool)
			}
		}
		for i := range h.cfg.OpenAICompatibility {
			cleanName := strings.ToLower(strings.TrimSpace(h.cfg.OpenAICompatibility[i].Name))
			cleanTarget := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(targetProvider), "openai-compatible-"), "openai-compatibility:")
			if req.Scope == "all" || strings.EqualFold(cleanName, targetProvider) || strings.EqualFold(cleanName, cleanTarget) {
				for j := range h.cfg.OpenAICompatibility[i].APIKeyEntries {
					h.cfg.OpenAICompatibility[i].APIKeyEntries[j].ProxyPool = computeNewPool(h.cfg.OpenAICompatibility[i].APIKeyEntries[j].ProxyPool)
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

// ResetProxyPoolCooldown clears the cooldown timer for all entries in a specified pool, or all pools.
func (h *Handler) ResetProxyPoolCooldown(c *gin.Context) {
	var req struct {
		Pool string `json:"pool"`
	}
	_ = bindFlexibleJSON(c, &req)
	poolName := strings.TrimSpace(req.Pool)

	reporter := h.getProxyPoolReporter()
	if reporter == nil {
		c.JSON(http.StatusOK, gin.H{"ok": true, "message": "no reporter available", "cleared": 0})
		return
	}

	cleared := 0
	if h.cfg != nil {
		for _, p := range h.cfg.ProxyPools {
			if poolName == "" || strings.EqualFold(strings.TrimSpace(p.Name), poolName) {
				for _, entry := range p.Entries {
					reporter.RecordEntrySuccess(p.Name, entry.URL)
					cleared++
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"cleared": cleared,
		"message": fmt.Sprintf("Cleared cooldown for %d proxy entries", cleared),
	})
}

func (h *Handler) persistProxyPools(c *gin.Context, status int, payload any) bool {
	if h != nil && h.authManager != nil {
		if updater, ok := h.authManager.RoundTripperProvider().(proxyPoolConfigUpdater); ok {
			updater.UpdateConfig(h.cfg)
		}
	}
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
