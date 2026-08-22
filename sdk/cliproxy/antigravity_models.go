package cliproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityModelBaseURLDaily = "https://daily-cloudcode-pa.googleapis.com"
	antigravityModelBaseURLProd  = "https://cloudcode-pa.googleapis.com"
	antigravityModelsPath        = "/v1internal:fetchAvailableModels"
)

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

func (s *Service) fetchAntigravityModelCapabilityHintsForAuth(ctx context.Context, auth *coreauth.Auth) antigravityModelCapabilityHints {
	if auth == nil || auth.Metadata == nil {
		return antigravityModelCapabilityHints{}
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return antigravityModelCapabilityHints{}
	}

	client := &http.Client{}
	if s != nil && s.coreManager != nil {
		if transport := s.coreManager.ProxyRoundTripper(auth); transport != nil {
			client.Transport = transport
		}
	}

	for _, baseURL := range antigravityModelBaseURLs(auth) {
		payload := antigravityModelsRequestPayload(auth)
		req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+antigravityModelsPath, strings.NewReader(payload))
		if errReq != nil {
			continue
		}
		req.Close = true
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("User-Agent", misc.AntigravityUserAgent())

		resp, errDo := client.Do(req)
		if errDo != nil {
			continue
		}
		body, errRead := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("antigravity model fetch: close response body: %v", errClose)
		}
		if errRead != nil {
			continue
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			continue
		}
		hints := parseAntigravityModelCapabilityHints(body)
		if len(hints.WebSearchModelIDs) > 0 || len(hints.Models) > 0 {
			return hints
		}
	}
	return antigravityModelCapabilityHints{}
}

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
	if baseURL := resolveAntigravityModelBaseURL(auth); baseURL != "" {
		return []string{baseURL}
	}
	return []string{antigravityModelBaseURLDaily, antigravityModelBaseURLProd}
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

func parseAntigravityModelCapabilityHints(body []byte) antigravityModelCapabilityHints {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return antigravityModelCapabilityHints{}
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
	return antigravityModelCapabilityHints{WebSearchModelIDs: webSearchModels, Models: models}
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
