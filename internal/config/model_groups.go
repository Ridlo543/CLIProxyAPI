package config

import (
	"fmt"
	"strings"
)

// ModelGroupMember identifies one ordered upstream target in a model group.
type ModelGroupMember struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
}

// ModelGroup target selection strategies.
const (
	// ModelGroupStrategyOrderedFallback tries targets strictly in configured order (default).
	ModelGroupStrategyOrderedFallback = "ordered-fallback"
	// ModelGroupStrategyRoundRobin rotates the starting target across requests,
	// then falls back through the remaining targets in order.
	ModelGroupStrategyRoundRobin = "round-robin"
)

// ModelGroup defines a client-visible model backed by ordered upstream targets.
type ModelGroup struct {
	Name     string             `yaml:"name" json:"name"`
	Strategy string             `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	Models   []ModelGroupMember `yaml:"models" json:"models"`
}

// NormalizeModelGroups validates model groups and returns a normalized copy.
func NormalizeModelGroups(groups []ModelGroup) ([]ModelGroup, error) {
	if len(groups) == 0 {
		return nil, nil
	}

	groupNames := make(map[string]struct{}, len(groups))
	for i := range groups {
		name := strings.TrimSpace(groups[i].Name)
		if name == "" {
			return nil, fmt.Errorf("model-groups[%d]: name must not be empty", i)
		}
		key := strings.ToLower(name)
		if _, exists := groupNames[key]; exists {
			return nil, fmt.Errorf("model-groups[%d]: duplicate group name %q", i, name)
		}
		groupNames[key] = struct{}{}
	}

	normalized := make([]ModelGroup, len(groups))
	for i, group := range groups {
		group.Name = strings.TrimSpace(group.Name)
		switch strings.ToLower(strings.TrimSpace(group.Strategy)) {
		case "":
			group.Strategy = ""
		case ModelGroupStrategyOrderedFallback:
			group.Strategy = ModelGroupStrategyOrderedFallback
		case ModelGroupStrategyRoundRobin:
			group.Strategy = ModelGroupStrategyRoundRobin
		default:
			return nil, fmt.Errorf("model-groups[%d] %q: strategy must be empty, %s, or %s", i, group.Name, ModelGroupStrategyOrderedFallback, ModelGroupStrategyRoundRobin)
		}
		if len(group.Models) == 0 {
			return nil, fmt.Errorf("model-groups[%d] %q: models must not be empty", i, group.Name)
		}
		seenMembers := make(map[string]struct{}, len(group.Models))
		group.Models = append([]ModelGroupMember(nil), group.Models...)
		for j := range group.Models {
			member := &group.Models[j]
			member.Provider = strings.ToLower(strings.TrimSpace(member.Provider))
			member.Model = strings.TrimSpace(member.Model)
			if member.Provider == "" || member.Model == "" {
				return nil, fmt.Errorf("model-groups[%d] %q models[%d]: provider and model must not be empty", i, group.Name, j)
			}
			if _, recursive := groupNames[strings.ToLower(member.Model)]; recursive {
				return nil, fmt.Errorf("model-groups[%d] %q models[%d]: model %q references a model group", i, group.Name, j, member.Model)
			}
			key := member.Provider + "\x00" + strings.ToLower(member.Model)
			if _, exists := seenMembers[key]; exists {
				return nil, fmt.Errorf("model-groups[%d] %q: duplicate member %s/%s", i, group.Name, member.Provider, member.Model)
			}
			seenMembers[key] = struct{}{}
		}
		normalized[i] = group
	}
	return normalized, nil
}
