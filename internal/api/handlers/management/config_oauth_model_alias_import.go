package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// knownOAuthModelAliasChannels mirrors the slugs returned by
// coreauth.OAuthModelAliasChannel for OAuth-backed providers ("gemini" is
// excluded because it maps to the empty channel).
var knownOAuthModelAliasChannels = map[string]struct{}{
	"claude": {}, "codex": {}, "vertex": {}, "antigravity": {},
	"aistudio": {}, "kimi": {}, "xai": {}, "qwen": {}, "iflow": {},
}

type oauthModelAliasInput struct {
	Name        string `json:"name"`
	Alias       string `json:"alias"`
	DisplayName string `json:"display-name"`
}

// AddOAuthModelAliases merges client-supplied aliases into one channel after
// validating names/aliases and rejecting duplicates against existing entries.
func (h *Handler) AddOAuthModelAliases(c *gin.Context) {
	channel := strings.ToLower(strings.TrimSpace(c.Param("channel")))
	if _, ok := knownOAuthModelAliasChannels[channel]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown oauth channel"})
		return
	}
	var body struct {
		Aliases []oauthModelAliasInput `json:"aliases"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || len(body.Aliases) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	existingAliases := make(map[string]struct{})
	for _, entry := range h.cfg.OAuthModelAlias[channel] {
		existingAliases[strings.ToLower(strings.TrimSpace(entry.Alias))] = struct{}{}
	}
	batch := make(map[string]struct{})
	var additions []config.OAuthModelAlias
	for _, input := range body.Aliases {
		name := strings.TrimSpace(input.Name)
		alias := strings.TrimSpace(input.Alias)
		if name == "" || alias == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and alias must not be empty"})
			return
		}
		key := strings.ToLower(alias)
		if _, dup := existingAliases[key]; dup {
			c.JSON(http.StatusConflict, gin.H{"error": "duplicate_alias", "alias": alias})
			return
		}
		if _, dup := batch[key]; dup {
			c.JSON(http.StatusConflict, gin.H{"error": "duplicate_alias", "alias": alias})
			return
		}
		batch[key] = struct{}{}
		additions = append(additions, config.OAuthModelAlias{Name: name, Alias: alias, DisplayName: strings.TrimSpace(input.DisplayName)})
	}

	if h.cfg.OAuthModelAlias == nil {
		h.cfg.OAuthModelAlias = make(map[string][]config.OAuthModelAlias)
	}
	h.cfg.OAuthModelAlias[channel] = append(h.cfg.OAuthModelAlias[channel], additions...)
	h.cfg.SanitizeOAuthModelAlias()
	snapshot, okSave := h.saveConfigAndSnapshotLocked(c)
	if !okSave {
		return
	}
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "added": len(additions), "skipped": len(body.Aliases) - len(additions)})
}

// GetOAuthModelAliasChannel lists current aliases for one channel.
func (h *Handler) GetOAuthModelAliasChannel(c *gin.Context) {
	channel := strings.ToLower(strings.TrimSpace(c.Param("channel")))
	if _, ok := knownOAuthModelAliasChannels[channel]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown oauth channel"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := sanitizedOAuthModelAlias(h.cfg.OAuthModelAlias)[channel]
	if entries == nil {
		entries = []config.OAuthModelAlias{}
	}
	c.JSON(http.StatusOK, gin.H{"channel": channel, "aliases": entries})
}
