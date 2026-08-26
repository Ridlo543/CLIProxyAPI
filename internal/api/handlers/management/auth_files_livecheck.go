package management

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestAuthFile reports the live health state of one credential file.
//
// POST /v0/management/auth-files/test  body: {"name": "<file or id>"}
//
// This is a STATE check assembled from the auth manager and the model
// registry: disabled flag, provider unavailability/cooldown, last execution
// error, and how many models the credential currently serves. It is NOT an
// active upstream ping — OAuth channels have per-provider protocols that a
// generic probe cannot speak. The response is always HTTP 200 with an
// {"ok": bool, ...} envelope so the UI can render a single verdict; only a
// missing credential gets a 404.
func (h *Handler) TestAuthFile(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "name is required"})
		return
	}

	var found *coreauth.Auth
	for _, a := range h.authManager.List() {
		if a.FileName == req.Name || a.ID == req.Name {
			found = a
			break
		}
	}
	if found == nil {
		c.JSON(http.StatusNotFound, gin.H{"ok": false, "error": "credential not found: " + req.Name})
		return
	}

	models := registry.GetGlobalRegistry().GetModelsForClient(found.ID)
	state := gin.H{
		"disabled":    found.Disabled,
		"unavailable": found.Unavailable,
	}
	// Report the underlying health even when the operator switch is off: an
	// explicit test must never be masked by the disabled flag alone.
	underlyingHealthy := !found.Unavailable && len(models) > 0
	var verdict string
	switch {
	case found.Disabled && found.Unavailable:
		verdict = "Disabled by operator, and currently unavailable"
		if found.StatusMessage != "" {
			verdict += ": " + found.StatusMessage
		}
	case found.Disabled && len(models) > 0:
		verdict = fmt.Sprintf("Disabled by operator — credential itself is healthy and serves %d models", len(models))
	case found.Disabled:
		verdict = "Disabled by operator — no models are currently registered for this credential"
	case found.Unavailable:
		verdict = "Temporarily unavailable"
		if found.StatusMessage != "" {
			verdict += ": " + found.StatusMessage
		}
	case len(models) == 0:
		verdict = "No models are currently registered for this credential"
	default:
		verdict = "Healthy — credential registered and serving models"
	}
	lastError := ""
	if found.LastError != nil {
		lastError = found.LastError.Message
		state["last_error"] = lastError
		if !found.Unavailable && len(models) > 0 {
			verdict += "; recent error: " + lastError
		}
	}

	ok := underlyingHealthy
	c.JSON(http.StatusOK, gin.H{
		"ok":                ok,
		"name":              found.FileName,
		"id":                found.ID,
		"provider":          found.Provider,
		"models_registered": len(models),
		"state":             state,
		"verdict":           verdict,
	})
}
