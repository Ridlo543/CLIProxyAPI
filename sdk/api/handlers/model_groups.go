package handlers

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// modelGroupRoundRobinCounters tracks one rotation counter per lowercase group
// name so round-robin groups start each request at the next target.
var modelGroupRoundRobinCounters sync.Map

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

// rotateTargets returns targets starting at start, wrapping around the end.
func rotateTargets(targets []coreauth.ModelGroupTarget, start uint64) []coreauth.ModelGroupTarget {
	if len(targets) == 0 {
		return targets
	}
	offset := int(start % uint64(len(targets)))
	rotated := make([]coreauth.ModelGroupTarget, 0, len(targets))
	rotated = append(rotated, targets[offset:]...)
	rotated = append(rotated, targets[:offset]...)
	return rotated
}

func modelGroupTargets(group config.ModelGroup) []coreauth.ModelGroupTarget {
	targets := make([]coreauth.ModelGroupTarget, 0, len(group.Models))
	for _, member := range group.Models {
		targets = append(targets, coreauth.ModelGroupTarget{Provider: member.Provider, Model: member.Model})
	}
	if strings.ToLower(strings.TrimSpace(group.Strategy)) == config.ModelGroupStrategyRoundRobin && len(targets) > 1 {
		key := strings.ToLower(strings.TrimSpace(group.Name))
		counterAny, _ := modelGroupRoundRobinCounters.LoadOrStore(key, &atomic.Uint64{})
		counter, _ := counterAny.(*atomic.Uint64)
		return rotateTargets(targets, counter.Add(1)-1)
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
