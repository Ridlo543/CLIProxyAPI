package management

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// The whole combos feature lives in this file plus internal/config/config_combos.go
// — deliberately isolated so pulling new CLIProxyAPI upstream commits stays
// conflict-free. Persistence reuses the shared persistLocked helper (comment-
// preserving save + async runtime reload).

// persistCombos saves under the held lock, schedules the async runtime
// reload, but leaves the RESPONSE to the caller — unlike persistLocked which
// emits its own {"status":"ok"} body. Returns false after writing an error.
func (h *Handler) persistCombos(c *gin.Context, status int, payload any) bool {
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config: " + err.Error()})
		return false
	}
	snapshot := h.reloadSnapshotConfigLocked()
	c.JSON(status, payload)
	var reqCtx context.Context
	if c != nil && c.Request != nil {
		reqCtx = c.Request.Context()
	}
	h.reloadConfigAfterManagementSaveAsync(reqCtx, snapshot)
	return true
}

// findCombo returns the index of the combo with the given (case-insensitive) name.
func findCombo(combos []config.ComboConfig, name string) int {
	target := strings.ToLower(strings.TrimSpace(name))
	for i := range combos {
		if strings.ToLower(combos[i].Name) == target {
			return i
		}
	}
	return -1
}

// ListCombos handles GET /v0/management/combos.
func (h *Handler) ListCombos(c *gin.Context) {
	out := make([]config.ComboConfig, 0, len(h.cfg.Combos))
	for _, cmb := range h.cfg.Combos {
		out = append(out, cmb.Clone())
	}
	c.JSON(http.StatusOK, gin.H{"combos": out})
}

// GetCombo handles GET /v0/management/combos/:name.
func (h *Handler) GetCombo(c *gin.Context) {
	idx := findCombo(h.cfg.Combos, c.Param("name"))
	if idx == -1 {
		c.JSON(http.StatusNotFound, gin.H{"error": "combo not found"})
		return
	}
	c.JSON(http.StatusOK, h.cfg.Combos[idx].Clone())
}

// CreateCombo handles POST /v0/management/combos.
func (h *Handler) CreateCombo(c *gin.Context) {
	var in config.ComboConfig
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	in.Normalize()
	if err := in.Validate(config.MinComboModels); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if findCombo(h.cfg.Combos, in.Name) != -1 {
		c.JSON(http.StatusConflict, gin.H{"error": "a combo named \"" + in.Name + "\" already exists"})
		return
	}
	h.cfg.Combos = append(h.cfg.Combos, in.Clone())
	last := h.cfg.Combos[len(h.cfg.Combos)-1].Clone()
	if !h.persistCombos(c, http.StatusCreated, last) {
		return
	}
}

// UpdateCombo handles PUT /v0/management/combos/:name (full replace; rename
// via the body's own name field).
func (h *Handler) UpdateCombo(c *gin.Context) {
	var in config.ComboConfig
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	in.Normalize()
	if err := in.Validate(config.MinComboModels); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	idx := findCombo(h.cfg.Combos, c.Param("name"))
	if idx == -1 {
		c.JSON(http.StatusNotFound, gin.H{"error": "combo not found"})
		return
	}
	if dup := findCombo(h.cfg.Combos, in.Name); dup != -1 && dup != idx {
		c.JSON(http.StatusConflict, gin.H{"error": "a combo named \"" + in.Name + "\" already exists"})
		return
	}
	h.cfg.Combos[idx] = in.Clone()
	saved := h.cfg.Combos[idx].Clone()
	if !h.persistCombos(c, http.StatusOK, saved) {
		return
	}
}

// DeleteCombo handles DELETE /v0/management/combos/:name.
func (h *Handler) DeleteCombo(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	idx := findCombo(h.cfg.Combos, c.Param("name"))
	if idx == -1 {
		c.JSON(http.StatusNotFound, gin.H{"error": "combo not found"})
		return
	}
	h.cfg.Combos = append(h.cfg.Combos[:idx], h.cfg.Combos[idx+1:]...)
	if !h.persistCombos(c, http.StatusOK, gin.H{"deleted": true}) {
		return
	}
}
