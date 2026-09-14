package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	antigravityModelBaseURLDaily      = "https://daily-cloudcode-pa.googleapis.com"
	antigravityModelBaseURLProd       = "https://cloudcode-pa.googleapis.com"
	antigravityModelsPath             = "/v1internal:fetchAvailableModels"
	antigravityCapabilityProbeTimeout = 5 * time.Second
	antigravityCapabilityCacheTTL     = 5 * time.Minute
	antigravityCapabilityFailureTTL   = 1 * time.Minute
)

type antigravityProbeStatus int

const (
	antigravityProbeStatusSuccess antigravityProbeStatus = iota
	antigravityProbeStatusAuthError
	antigravityProbeStatusTransientError
)

type antigravityCapabilityCacheEntry struct {
	hints     antigravityModelCapabilityHints
	expiresAt time.Time
}

type antigravityProbeResult struct {
	hints  antigravityModelCapabilityHints
	status antigravityProbeStatus
	token  string
}

var (
	antigravityNowFunc          = time.Now
	antigravityCapabilityMu     sync.RWMutex
	antigravityCapabilityCache  = make(map[string]antigravityCapabilityCacheEntry)
	antigravityAuthFailureCache = make(map[string]time.Time)
	antigravityCapabilityGroup  singleflight.Group
	antigravityProbeInFlight    atomic.Int32
)

func purgeExpiredAntigravityCacheLocked(now time.Time) {
	for k, v := range antigravityCapabilityCache {
		if !now.Before(v.expiresAt) {
			delete(antigravityCapabilityCache, k)
		}
	}
	for k, exp := range antigravityAuthFailureCache {
		if !now.Before(exp) {
			delete(antigravityAuthFailureCache, k)
		}
	}
}

type antigravityFetchAvailableModelsResponse struct {
	WebSearchModelIDs []string `json:"webSearchModelIds"`
	Models            map[string]struct {
		DisplayName     string `json:"displayName"`
		MaxTokens       int    `json:"maxTokens"`
		MaxOutputTokens int    `json:"maxOutputTokens"`
	} `json:"models"`
}

type antigravityModelCapabilityHints struct {
	WebSearchModelIDs map[string]struct{}
	Models            []*ModelInfo
}

// clone must carry Models too: the capability cache serves every probe after the
// first one, and a clone without them would drop the live account catalog.
func (h antigravityModelCapabilityHints) clone() antigravityModelCapabilityHints {
	var out antigravityModelCapabilityHints
	if h.WebSearchModelIDs != nil {
		out.WebSearchModelIDs = make(map[string]struct{}, len(h.WebSearchModelIDs))
		for k := range h.WebSearchModelIDs {
			out.WebSearchModelIDs[k] = struct{}{}
		}
	}
	if h.Models != nil {
		out.Models = make([]*ModelInfo, 0, len(h.Models))
		for _, model := range h.Models {
			if model == nil {
				continue
			}
			copied := *model
			out.Models = append(out.Models, &copied)
		}
	}
	return out
}

func (s *Service) fetchAntigravityModelCapabilityHintsForAuth(ctx context.Context, auth *coreauth.Auth) antigravityModelCapabilityHints {
	if auth == nil || auth.Metadata == nil {
		return antigravityModelCapabilityHints{}
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return antigravityModelCapabilityHints{}
	}

	baseURLs := antigravityModelBaseURLs(auth)
	if len(baseURLs) == 0 {
		return antigravityModelCapabilityHints{}
	}

	proxyURL := s.antigravityModelFetchProxyURL(auth)
	// The probe payload is scoped to the credential project, so accounts on
	// different projects must not share a cached answer.
	cacheKey := strings.Join(baseURLs, "|") + "#" + proxyURL + "#" + antigravityModelsRequestPayload(auth)
	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(accessToken)))
	authFailKey := auth.ID + "#" + tokenHash

	now := antigravityNowFunc()
	antigravityCapabilityMu.RLock()
	if failExpiry, failed := antigravityAuthFailureCache[authFailKey]; failed && now.Before(failExpiry) {
		antigravityCapabilityMu.RUnlock()
		return antigravityModelCapabilityHints{}
	}
	if entry, ok := antigravityCapabilityCache[cacheKey]; ok && now.Before(entry.expiresAt) {
		hints := entry.hints.clone()
		antigravityCapabilityMu.RUnlock()
		return hints
	}
	antigravityCapabilityMu.RUnlock()

	antigravityProbeInFlight.Add(1)
	defer antigravityProbeInFlight.Add(-1)

	res, errDo, _ := antigravityCapabilityGroup.Do(cacheKey, func() (any, error) {
		nowInside := antigravityNowFunc()
		antigravityCapabilityMu.RLock()
		if entry, ok := antigravityCapabilityCache[cacheKey]; ok && nowInside.Before(entry.expiresAt) {
			hints := entry.hints.clone()
			antigravityCapabilityMu.RUnlock()
			return antigravityProbeResult{hints: hints, status: antigravityProbeStatusSuccess, token: accessToken}, nil
		}
		antigravityCapabilityMu.RUnlock()

		hints, status := s.probeAntigravityModelCapabilityHints(ctx, auth, baseURLs, proxyURL, accessToken)
		if status == antigravityProbeStatusSuccess || status == antigravityProbeStatusTransientError {
			if status != antigravityProbeStatusTransientError || ctx == nil || ctx.Err() == nil {
				ttl := antigravityCapabilityCacheTTL
				if status != antigravityProbeStatusSuccess {
					ttl = antigravityCapabilityFailureTTL
				}
				antigravityCapabilityMu.Lock()
				purgeExpiredAntigravityCacheLocked(nowInside)
				antigravityCapabilityCache[cacheKey] = antigravityCapabilityCacheEntry{
					hints:     hints.clone(),
					expiresAt: antigravityNowFunc().Add(ttl),
				}
				antigravityCapabilityMu.Unlock()
			}
		}
		return antigravityProbeResult{hints: hints, status: status, token: accessToken}, nil
	})
	if errDo != nil || res == nil {
		return antigravityModelCapabilityHints{}
	}
	result, ok := res.(antigravityProbeResult)
	if !ok {
		return antigravityModelCapabilityHints{}
	}

	if result.status == antigravityProbeStatusAuthError {
		if result.token != accessToken {
			// The shared probe failed with an auth error from another account's token.
			// Re-try with our own account's token.
			return s.fetchAntigravityModelCapabilityHintsForAuth(ctx, auth)
		}
		// Our own token failed with 401/403. Record backoff for this account/token.
		antigravityCapabilityMu.Lock()
		purgeExpiredAntigravityCacheLocked(now)
		antigravityAuthFailureCache[authFailKey] = antigravityNowFunc().Add(antigravityCapabilityFailureTTL)
		antigravityCapabilityMu.Unlock()
		return antigravityModelCapabilityHints{}
	}

	return result.hints.clone()
}

func (s *Service) probeAntigravityModelCapabilityHints(ctx context.Context, auth *coreauth.Auth, baseURLs []string, proxyURL string, accessToken string) (antigravityModelCapabilityHints, antigravityProbeStatus) {
	probeCtx := context.Background()
	var cancel context.CancelFunc
	probeCtx, cancel = context.WithTimeout(probeCtx, antigravityCapabilityProbeTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: antigravityCapabilityProbeTimeout,
	}
	// Resolve the transport through the auth manager so per-credential proxy pools
	// apply to the capability probe as well.
	if s != nil && s.coreManager != nil {
		if transport := s.coreManager.ProxyRoundTripper(auth); transport != nil {
			client.Transport = transport
		}
	} else if transport, _, errProxy := proxyutil.BuildHTTPTransport(proxyURL); errProxy == nil && transport != nil {
		client.Transport = transport
	}

	if len(baseURLs) == 1 {
		return s.fetchAntigravityModelHintsFromURL(probeCtx, client, baseURLs[0], accessToken, auth)
	}

	type probeResult struct {
		hints  antigravityModelCapabilityHints
		status antigravityProbeStatus
	}
	ch := make(chan probeResult, len(baseURLs))
	for _, baseURL := range baseURLs {
		go func(url string) {
			h, status := s.fetchAntigravityModelHintsFromURL(probeCtx, client, url, accessToken, auth)
			ch <- probeResult{hints: h, status: status}
		}(baseURL)
	}

	var firstSuccess antigravityModelCapabilityHints
	var hadSuccess bool
	overallStatus := antigravityProbeStatusTransientError
	for i := 0; i < len(baseURLs); i++ {
		select {
		case <-probeCtx.Done():
			return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
		case res := <-ch:
			if res.status == antigravityProbeStatusSuccess {
				if len(res.hints.WebSearchModelIDs) > 0 || len(res.hints.Models) > 0 {
					return res.hints, antigravityProbeStatusSuccess
				}
				if !hadSuccess {
					firstSuccess = res.hints
					hadSuccess = true
				}
			} else if res.status == antigravityProbeStatusAuthError {
				overallStatus = antigravityProbeStatusAuthError
			}
		}
	}
	if hadSuccess {
		return firstSuccess, antigravityProbeStatusSuccess
	}
	return antigravityModelCapabilityHints{}, overallStatus
}

func (s *Service) fetchAntigravityModelHintsFromURL(ctx context.Context, client *http.Client, baseURL string, accessToken string, auth *coreauth.Auth) (antigravityModelCapabilityHints, antigravityProbeStatus) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+antigravityModelsPath, strings.NewReader(antigravityModelsRequestPayload(auth)))
	if errReq != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	req.Close = true
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", misc.AntigravityUserAgent())

	resp, errDo := client.Do(req)
	if errDo != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("antigravity model fetch: close response body: %v", errClose)
		}
	}()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusAuthError
	}
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	hints, ok := parseAntigravityModelCapabilityHints(body)
	if !ok {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	return hints, antigravityProbeStatusSuccess
}

func (s *Service) antigravityModelFetchProxyURL(auth *coreauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if s != nil {
		s.cfgMu.RLock()
		defer s.cfgMu.RUnlock()
		if s.cfg != nil {
			return strings.TrimSpace(s.cfg.ProxyURL)
		}
	}
	return ""
}

// antigravityModelsRequestPayload scopes the probe to the credential project so
// fetchAvailableModels answers with that account's catalog instead of an empty set.
func antigravityModelsRequestPayload(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return `{}`
	}
	projectID, _ := auth.Metadata["project_id"].(string)
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return `{}`
	}
	encodedProjectID, errMarshal := json.Marshal(projectID)
	if errMarshal != nil {
		return `{}`
	}
	return "{\"project\":" + string(encodedProjectID) + "}"
}

func antigravityModelBaseURLs(auth *coreauth.Auth) []string {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["base_urls"]); raw != "" {
			parts := strings.Split(raw, ",")
			urls := make([]string, 0, len(parts))
			for _, p := range parts {
				if trimmed := strings.TrimRight(strings.TrimSpace(p), "/"); trimmed != "" {
					urls = append(urls, trimmed)
				}
			}
			if len(urls) > 0 {
				return urls
			}
		}
	}
	if baseURL := resolveAntigravityModelBaseURL(auth); baseURL != "" {
		return []string{baseURL}
	}
	return []string{antigravityModelBaseURLDaily}
}

func resolveAntigravityModelBaseURL(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["base_url"]); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["base_url"].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				return strings.TrimRight(value, "/")
			}
		}
	}
	return ""
}

func parseAntigravityModelCapabilityHints(body []byte) (antigravityModelCapabilityHints, bool) {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return antigravityModelCapabilityHints{}, false
	}
	webSearchModels := make(map[string]struct{}, len(parsed.WebSearchModelIDs))
	for _, modelID := range parsed.WebSearchModelIDs {
		modelID = normalizeAntigravityFetchedModelID(modelID)
		if modelID != "" {
			webSearchModels[modelID] = struct{}{}
		}
	}
	modelIDs := make([]string, 0, len(parsed.Models))
	for modelID := range parsed.Models {
		modelIDs = append(modelIDs, modelID)
	}
	sort.Strings(modelIDs)
	models := make([]*ModelInfo, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" || isInternalAntigravityModel(modelID) {
			continue
		}
		upstream := parsed.Models[modelID]
		displayName := strings.TrimSpace(upstream.DisplayName)
		if displayName == "" {
			displayName = modelID
		}
		models = append(models, &ModelInfo{
			ID:                  modelID,
			Object:              "model",
			Created:             time.Now().Unix(),
			OwnedBy:             "antigravity",
			Type:                "antigravity",
			DisplayName:         displayName,
			Name:                modelID,
			Description:         displayName,
			ContextLength:       upstream.MaxTokens,
			MaxContextLength:    upstream.MaxTokens,
			MaxCompletionTokens: upstream.MaxOutputTokens,
		})
	}
	return antigravityModelCapabilityHints{WebSearchModelIDs: webSearchModels, Models: models}, true
}

func applyAntigravityFetchedModelCapabilities(models []*ModelInfo, hints antigravityModelCapabilityHints) []*ModelInfo {
	merged := make([]*ModelInfo, 0, len(models)+len(hints.Models))
	modelsByID := make(map[string]*ModelInfo, len(models)+len(hints.Models))
	for _, model := range models {
		if model == nil {
			continue
		}
		clone := *model
		merged = append(merged, &clone)
		modelsByID[normalizeAntigravityFetchedModelID(clone.ID)] = &clone
	}
	for _, fetched := range hints.Models {
		if fetched == nil {
			continue
		}
		key := normalizeAntigravityFetchedModelID(fetched.ID)
		if existing := modelsByID[key]; existing != nil {
			if fetched.DisplayName != "" {
				existing.DisplayName = fetched.DisplayName
			}
			continue
		}
		clone := *fetched
		merged = append(merged, &clone)
		modelsByID[key] = &clone
	}

	for _, model := range merged {
		modelID := normalizeAntigravityFetchedModelID(model.ID)
		if _, ok := hints.WebSearchModelIDs[modelID]; ok {
			model.SupportsWebSearch = true
		}
	}
	return merged
}

func isInternalAntigravityModel(modelID string) bool {
	switch modelID {
	case "chat_20706", "chat_23310", "tab_flash_lite_preview", "tab_jump_flash_lite_preview", "gemini-2.5-flash-thinking", "gemini-2.5-pro":
		return true
	default:
		return false
	}
}

func normalizeAntigravityFetchedModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

// WaitAntigravityProbes waits for any in-flight asynchronous Antigravity capability probes to complete.
func (s *Service) WaitAntigravityProbes() {
	if s == nil {
		return
	}
	s.antigravityProbeWg.Wait()
}

func (s *Service) waitAntigravityProbesContext(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		s.antigravityProbeWg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

func (s *Service) asyncProbeAntigravityCapabilities(ctx context.Context, auth *coreauth.Auth, providerKey string) {
	if auth == nil || auth.ID == "" || auth.Disabled {
		return
	}
	authClone := auth.Clone()
	expectedEpoch := auth.RegistrationEpoch
	expectedPrefix := auth.Prefix
	expectedRegEpoch := GlobalModelRegistry().ClientRegistrationEpoch(auth.ID)

	probeCtx := ctx
	if probeCtx == nil {
		probeCtx = context.Background()
	}

	if s != nil {
		s.antigravityProbeWg.Add(1)
	}

	go func() {
		if s != nil {
			defer s.antigravityProbeWg.Done()
		}
		hints := s.fetchAntigravityModelCapabilityHintsForAuth(probeCtx, authClone)
		if len(hints.WebSearchModelIDs) == 0 {
			return
		}
		if s == nil {
			return
		}
		if s.coreManager != nil {
			current, exists := s.coreManager.GetByID(authClone.ID)
			if !exists || current == nil || current.Disabled {
				return
			}
			// Version protection: verify auth registration epoch and prefix did not change.
			// Auth.Generation is intentionally NOT checked here because regular request completions
			// increment Generation on every request (via MarkResult), which must not invalidate valid probe results.
			if current.RegistrationEpoch != expectedEpoch || current.Prefix != expectedPrefix {
				return
			}
		}
		aliasMap := s.buildAntigravityReverseAliasMap(authClone)

		// Atomically update capabilities on existing registered models if epoch matches
		updated := GlobalModelRegistry().ApplyClientModelCapabilities(authClone.ID, expectedRegEpoch, func(modelID string, info *ModelInfo) {
			upstreamID := resolveAntigravityUpstreamModelID(modelID, authClone.Prefix, aliasMap)
			if _, ok := hints.WebSearchModelIDs[upstreamID]; ok {
				info.SupportsWebSearch = true
			}
		})
		if !updated {
			return
		}

		if s.coreManager != nil {
			s.coreManager.ReconcileRegistryModelStates(context.Background(), authClone.ID)
			s.coreManager.RefreshSchedulerEntry(authClone.ID)
		}
	}()
}

func (s *Service) buildAntigravityReverseAliasMap(auth *coreauth.Auth) map[string]string {
	if auth == nil {
		return nil
	}
	var cfg *config.Config
	if s != nil {
		s.cfgMu.RLock()
		cfg = s.cfg
		s.cfgMu.RUnlock()
	}
	channel := coreauth.OAuthModelAliasChannel(auth.Provider, auth.AuthKind())
	aliases := oauthModelAliasesForAuth(cfg, channel, auth.Attributes)
	if len(aliases) == 0 {
		return nil
	}
	aliasMap := make(map[string]string, len(aliases))
	for _, entry := range aliases {
		aliasName := strings.ToLower(strings.TrimSpace(entry.Alias))
		upstreamName := strings.ToLower(strings.TrimSpace(entry.Name))
		if aliasName != "" && upstreamName != "" {
			aliasMap[aliasName] = upstreamName
		}
	}
	return aliasMap
}

func resolveAntigravityUpstreamModelID(modelID string, prefix string, aliasMap map[string]string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	prefix = strings.ToLower(strings.Trim(strings.TrimSpace(prefix), "/"))
	unprefixed := modelID
	if prefix != "" && strings.HasPrefix(modelID, prefix+"/") {
		unprefixed = modelID[len(prefix)+1:]
	}
	if len(aliasMap) > 0 {
		if upstream, ok := aliasMap[unprefixed]; ok && upstream != "" {
			return upstream
		}
	}
	return unprefixed
}
