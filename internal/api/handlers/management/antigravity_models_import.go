package management

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// ImportAntigravityModels probes active Antigravity credentials against Google Cloud Code
// endpoints (/v1internal:fetchAvailableModels) to dynamically discover and register all
// available upstream models (e.g. Claude Opus 5.5, Gemini 3.x).
func (h *Handler) ImportAntigravityModels(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth manager not available"})
		return
	}

	auths := h.authManager.List()
	var candidates []*coreauth.Auth
	for _, a := range auths {
		if a == nil || a.Disabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(a.Provider), "antigravity") {
			candidates = append(candidates, a)
		}
	}

	if len(candidates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no active antigravity accounts found to probe upstream"})
		return
	}

	endpoints := []string{
		"https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
	}

	client := &http.Client{Timeout: 15 * time.Second}
	discoveredMap := make(map[string]*registry.ModelInfo)
	var probedAccounts []string

	for _, a := range candidates {
		token := ""
		if a.Metadata != nil {
			if t, ok := a.Metadata["access_token"].(string); ok && t != "" {
				token = t
			} else if t, ok := a.Metadata["accessToken"].(string); ok && t != "" {
				token = t
			}
		}
		if token == "" && a.Attributes != nil {
			token = a.Attributes["access_token"]
		}
		if token == "" {
			continue
		}
		accountLabel := authEmail(a)
		if accountLabel == "" {
			accountLabel = a.ID
		}
		probedAccounts = append(probedAccounts, accountLabel)

		for _, ep := range endpoints {
			req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, ep, bytes.NewReader([]byte("{}")))
			if err != nil {
				continue
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "antigravity/cli/1.0.13 (aidev_client; os_type=linux; arch=x86_64)")

			resp, err := client.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				if resp != nil {
					_ = resp.Body.Close()
				}
				continue
			}

			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				continue
			}

			var parsed struct {
				WebSearchModelIDs []string `json:"webSearchModelIds"`
				Models            map[string]struct {
					DisplayName     string `json:"displayName"`
					MaxTokens       int    `json:"maxTokens"`
					MaxOutputTokens int    `json:"maxOutputTokens"`
				} `json:"models"`
			}
			if err := json.Unmarshal(body, &parsed); err != nil {
				continue
			}

			webSearchSet := make(map[string]bool, len(parsed.WebSearchModelIDs))
			for _, ws := range parsed.WebSearchModelIDs {
				webSearchSet[strings.ToLower(strings.TrimSpace(ws))] = true
			}

			for modelID, upstream := range parsed.Models {
				modelID = strings.TrimSpace(modelID)
				if modelID == "" || isInternalAntigravityModelID(modelID) {
					continue
				}
				key := strings.ToLower(modelID)
				if _, exists := discoveredMap[key]; exists {
					continue
				}

				displayName := strings.TrimSpace(upstream.DisplayName)
				if displayName == "" {
					displayName = modelID
				}

				ctxLen := upstream.MaxTokens
				if ctxLen <= 0 {
					if strings.Contains(key, "claude") {
						ctxLen = 1000000
					} else {
						ctxLen = 1048576
					}
				}

				maxOut := upstream.MaxOutputTokens
				if maxOut <= 0 {
					if strings.Contains(key, "claude") {
						maxOut = 128000
					} else {
						maxOut = 65536
					}
				}

				var thinking *registry.ThinkingSupport
				if strings.Contains(key, "thinking") || strings.Contains(key, "high") || strings.Contains(key, "medium") || strings.Contains(key, "low") {
					thinking = &registry.ThinkingSupport{
						Min:            1024,
						Max:            64000,
						ZeroAllowed:    true,
						DynamicAllowed: true,
					}
				}

				discoveredMap[key] = &registry.ModelInfo{
					ID:                  modelID,
					Object:              "model",
					Created:             time.Now().Unix(),
					OwnedBy:             "antigravity",
					Type:                "antigravity",
					DisplayName:         displayName,
					Name:                modelID,
					Description:         displayName,
					ContextLength:       ctxLen,
					MaxContextLength:    ctxLen,
					MaxCompletionTokens: maxOut,
					Thinking:            thinking,
					SupportsWebSearch:   webSearchSet[key],
					SupportedInputModalities:  []string{"text", "image"},
					SupportedOutputModalities: []string{"text"},
				}
			}
		}
	}

	var newModels []*registry.ModelInfo
	for _, m := range discoveredMap {
		newModels = append(newModels, m)
	}

	added := registry.AppendAntigravityModels(newModels)

	// Re-register models for all active Antigravity auths so the new models are immediately usable
	reg := registry.GetGlobalRegistry()
	allAntigravity := registry.GetAntigravityModels()
	for _, a := range candidates {
		reg.RegisterClient(a.ID, a.Provider, allAntigravity)
	}

	log.Infof("[management] imported %d new Antigravity models from upstream: %v", len(added), added)

	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"added":           added,
		"total":           len(allAntigravity),
		"probed_accounts": probedAccounts,
		"models":          newModels,
	})
}

func isInternalAntigravityModelID(modelID string) bool {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "chat_20706", "chat_23310", "tab_flash_lite_preview", "tab_jump_flash_lite_preview", "gemini-2.5-flash-thinking", "gemini-2.5-pro":
		return true
	default:
		return false
	}
}
