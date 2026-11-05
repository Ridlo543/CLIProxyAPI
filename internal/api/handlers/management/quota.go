package management

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Quota exceeded toggles
func (h *Handler) GetSwitchProject(c *gin.Context) {
	c.JSON(200, gin.H{"switch-project": h.cfg.QuotaExceeded.SwitchProject})
}
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchProject = v })
}

func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	c.JSON(200, gin.H{"switch-preview-model": h.cfg.QuotaExceeded.SwitchPreviewModel})
}
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchPreviewModel = v })
}

// ResetQuota clears quota/cooldown routing state for one auth index.
func (h *Handler) ResetQuota(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	if errBindJSON := c.ShouldBindJSON(&req); errBindJSON != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(errBindJSON, http.StatusBadRequest), gin.H{"error": "invalid request body"})
		return
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndexStrict(authIndex)
	if auth == nil {
		if !strings.HasSuffix(strings.ToLower(authIndex), ".json") {
			lowerTarget := strings.ToLower(authIndex)
			var matched []*coreauth.Auth
			for _, a := range h.authManager.List() {
				if a == nil || strings.EqualFold(a.ID, authIndex) || strings.EqualFold(a.FileName, authIndex) {
					continue
				}
				cleanProv := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(a.Provider), "openai-compatible-"), "openai-compatibility:")
				if cleanProv == lowerTarget || strings.EqualFold(a.Provider, authIndex) || strings.EqualFold(a.Label, authIndex) {
					matched = append(matched, a)
				} else if strings.HasPrefix(strings.ToLower(a.ID), "openai-compatibility:"+lowerTarget+":") {
					matched = append(matched, a)
				}
			}
			if len(matched) > 0 {
				var allModels []string
				seenModels := make(map[string]bool)
				for _, a := range matched {
					if _, mList, err := h.authManager.ResetQuota(c.Request.Context(), a.ID); err == nil {
						for _, m := range mList {
							if !seenModels[m] {
								seenModels[m] = true
								allModels = append(allModels, m)
							}
						}
					}
				}
				c.JSON(http.StatusOK, gin.H{
					"status":     "ok",
					"auth_index": authIndex,
					"models":     allModels,
				})
				return
			}
		}

		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	updated, models, errReset := h.authManager.ResetQuota(c.Request.Context(), auth.ID)
	if errReset != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to reset quota: %v", errReset)})
		return
	}
	if updated == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	updated.EnsureIndex()

	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"auth_index": updated.Index,
		"models":     models,
	})
}
