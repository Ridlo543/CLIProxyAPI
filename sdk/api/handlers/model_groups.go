package handlers

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func (h *BaseAPIHandler) configuredModelGroup(name string) (config.ModelGroup, bool) {
	if h == nil || h.Cfg == nil {
		return config.ModelGroup{}, false
	}
	name = strings.TrimSpace(name)
	for _, group := range h.Cfg.ModelGroups {
		if strings.EqualFold(group.Name, name) {
			return group, true
		}
	}
	return config.ModelGroup{}, false
}

func modelGroupTargets(group config.ModelGroup) []coreauth.ModelGroupTarget {
	targets := make([]coreauth.ModelGroupTarget, 0, len(group.Models))
	for _, member := range group.Models {
		targets = append(targets, coreauth.ModelGroupTarget{Provider: member.Provider, Model: member.Model})
	}
	return targets
}

// ModelGroupModels returns configured groups in the requested protocol's native listing shape.
func (h *BaseAPIHandler) ModelGroupModels(protocol string) []map[string]any {
	if h == nil || h.Cfg == nil {
		return nil
	}
	models := make([]map[string]any, 0, len(h.Cfg.ModelGroups))
	for _, group := range h.Cfg.ModelGroups {
		switch strings.ToLower(strings.TrimSpace(protocol)) {
		case "claude":
			models = append(models, map[string]any{"id": group.Name, "object": "model", "type": "model", "display_name": group.Name, "owned_by": "ainyrouter/model-group"})
		case "gemini":
			models = append(models, map[string]any{"name": group.Name, "displayName": group.Name, "description": "ainyrouter/model-group", "supportedGenerationMethods": []string{"generateContent"}})
		default:
			models = append(models, map[string]any{"id": group.Name, "object": "model", "owned_by": "ainyrouter/model-group", "type": "model-group"})
		}
	}
	return models
}
