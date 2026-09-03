package config

import (
	"fmt"
	"strings"
)

// ComboStrategy enumerates how a combo picks among its member models.
type ComboStrategy string

const (
	// ComboStrategyFallback tries members in listed order; first success wins,
	// failures fall through to the next candidate.
	ComboStrategyFallback ComboStrategy = "fallback"
	// ComboStrategyRoundRobin rotates the chain head per request so traffic is
	// spread across members; failures still fall through to the next member.
	ComboStrategyRoundRobin ComboStrategy = "round-robin"
)

// ComboModelRef references one concrete upstream model as "provider/model",
// mirroring 9Router's combo chain entries (e.g. "ag/claude-sonnet-4-5").
// provider must match a provider identifier known to the router
// (oauth channel, openai-compatibility name, or keyless listing id).
type ComboModelRef struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
}

// ComboConfig is one named combination of models callable as if it were a
// single model by clients (e.g. model: "leader" in /v1/chat/completions).
//
// The whole combos feature lives in this file plus internal/combos and the
// combos_crud.go management handler — deliberately isolated so pulling new
// CLIProxyAPI upstream commits stays conflict-free.
type ComboConfig struct {
	Name                string          `yaml:"name" json:"name"`
	Strategy            ComboStrategy   `yaml:"strategy" json:"strategy"`
	Models              []ComboModelRef `yaml:"models" json:"models"`
	ContextLength       int             `yaml:"context_length,omitempty" json:"context_length,omitempty"`
	MaxTokens           int             `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `yaml:"max_completion_tokens,omitempty" json:"max_completion_tokens,omitempty"`
}

// Normalize trims whitespace and lowercases the strategy.
func (c *ComboConfig) Normalize() {
	c.Name = strings.TrimSpace(c.Name)
	c.Strategy = ComboStrategy(strings.ToLower(strings.TrimSpace(string(c.Strategy))))
	if c.Strategy == "" {
		c.Strategy = ComboStrategyFallback
	}
	for i := range c.Models {
		c.Models[i].Provider = strings.TrimSpace(c.Models[i].Provider)
		c.Models[i].Model = strings.TrimSpace(c.Models[i].Model)
	}
}

// Validate reports whether the combo is safe to persist. minModels guards
// against degenerate chains (9Router requires at least two members too).
func (c *ComboConfig) Validate(minModels int) error {
	if c.Name == "" {
		return fmt.Errorf("combo name is required")
	}
	if len(c.Name) > 64 {
		return fmt.Errorf("combo name must be at most 64 characters")
	}
	switch c.Strategy {
	case ComboStrategyFallback, ComboStrategyRoundRobin:
	default:
		return fmt.Errorf("unknown strategy %q (use fallback or round-robin)", string(c.Strategy))
	}
	if len(c.Models) < minModels {
		return fmt.Errorf("a combo needs at least %d models", minModels)
	}
	seen := make(map[string]struct{}, len(c.Models))
	for i, m := range c.Models {
		if m.Provider == "" || m.Model == "" {
			return fmt.Errorf("models[%d]: provider and model are required", i)
		}
		key := strings.ToLower(m.Provider + "/" + m.Model)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("models[%d]: duplicate entry %q", i, m.Provider+"/"+m.Model)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// Clone returns a deep copy so handlers never share slices with h.cfg.
func (c ComboConfig) Clone() ComboConfig {
	out := c
	out.Models = append([]ComboModelRef(nil), c.Models...)
	return out
}

// CapacityAdapterPoolConfig defines an auto-switching model pool for a specific input modality (e.g. vision).
type CapacityAdapterPoolConfig struct {
	Enabled    bool            `yaml:"enabled" json:"enabled"`
	RoundRobin bool            `yaml:"round_robin" json:"round_robin"`
	Models     []ComboModelRef `yaml:"models" json:"models"`
}

// CapacityAdapterConfig holds capacity adapter settings across modalities.
type CapacityAdapterConfig struct {
	Vision CapacityAdapterPoolConfig `yaml:"vision" json:"vision"`
	Audio  CapacityAdapterPoolConfig `yaml:"audio" json:"audio"`
}

func (c *CapacityAdapterConfig) Normalize() {
	if c.Vision.Models == nil {
		c.Vision.Models = []ComboModelRef{}
	}
	if c.Audio.Models == nil {
		c.Audio.Models = []ComboModelRef{}
	}
	for i := range c.Vision.Models {
		c.Vision.Models[i].Provider = strings.TrimSpace(c.Vision.Models[i].Provider)
		c.Vision.Models[i].Model = strings.TrimSpace(c.Vision.Models[i].Model)
	}
	for i := range c.Audio.Models {
		c.Audio.Models[i].Provider = strings.TrimSpace(c.Audio.Models[i].Provider)
		c.Audio.Models[i].Model = strings.TrimSpace(c.Audio.Models[i].Model)
	}
}

func (c CapacityAdapterConfig) Clone() CapacityAdapterConfig {
	out := c
	if c.Vision.Models != nil {
		out.Vision.Models = append([]ComboModelRef(nil), c.Vision.Models...)
	} else {
		out.Vision.Models = []ComboModelRef{}
	}
	if c.Audio.Models != nil {
		out.Audio.Models = append([]ComboModelRef(nil), c.Audio.Models...)
	} else {
		out.Audio.Models = []ComboModelRef{}
	}
	return out
}

// MinComboModels is the smallest useful combo size.
const MinComboModels = 2
