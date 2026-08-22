package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

const DefaultProxyPoolCooldown = 30 * time.Second

type ProxyPool struct {
	Name     string           `yaml:"name" json:"name"`
	Strategy string           `yaml:"strategy" json:"strategy"`
	Strict   bool             `yaml:"strict" json:"strict"`
	Cooldown string           `yaml:"cooldown,omitempty" json:"cooldown,omitempty"`
	Entries  []ProxyPoolEntry `yaml:"entries" json:"entries"`
}

type ProxyPoolEntry struct {
	URL string `yaml:"url" json:"url"`
}

func (cfg *Config) HasProxyPool(name string) bool {
	name = strings.TrimSpace(name)
	if cfg == nil || name == "" {
		return false
	}
	for i := range cfg.ProxyPools {
		if strings.EqualFold(strings.TrimSpace(cfg.ProxyPools[i].Name), name) {
			return true
		}
	}
	return false
}

func (cfg *Config) NormalizeProxyPools() error {
	if cfg == nil {
		return nil
	}
	names := make(map[string]struct{}, len(cfg.ProxyPools))
	for i := range cfg.ProxyPools {
		pool := &cfg.ProxyPools[i]
		pool.Name = strings.TrimSpace(pool.Name)
		pool.Strategy = strings.ToLower(strings.TrimSpace(pool.Strategy))
		pool.Cooldown = strings.TrimSpace(pool.Cooldown)
		if pool.Name == "" {
			return fmt.Errorf("proxy-pools[%d]: name must not be empty", i)
		}
		key := strings.ToLower(pool.Name)
		if _, exists := names[key]; exists {
			return fmt.Errorf("proxy-pools[%d]: duplicate name %q", i, pool.Name)
		}
		names[key] = struct{}{}
		switch pool.Strategy {
		case "ordered-fallback", "round-robin", "random":
		default:
			return fmt.Errorf("proxy pool %q: invalid strategy %q", pool.Name, pool.Strategy)
		}
		if len(pool.Entries) == 0 {
			return fmt.Errorf("proxy pool %q: entries must not be empty", pool.Name)
		}
		if pool.Cooldown != "" {
			d, errDuration := time.ParseDuration(pool.Cooldown)
			if errDuration != nil || d <= 0 {
				return fmt.Errorf("proxy pool %q: cooldown must be a positive duration", pool.Name)
			}
		}
		seenEntries := make(map[string]struct{}, len(pool.Entries))
		for j := range pool.Entries {
			pool.Entries[j].URL = strings.TrimSpace(pool.Entries[j].URL)
			setting, errParse := proxyutil.Parse(pool.Entries[j].URL)
			isDirect := strings.EqualFold(pool.Entries[j].URL, "direct")
			if errParse != nil || (!isDirect && setting.Mode != proxyutil.ModeProxy) {
				return fmt.Errorf("proxy pool %q entry %d: invalid proxy URL", pool.Name, j)
			}
			entryKey := strings.ToLower(pool.Entries[j].URL)
			if _, exists := seenEntries[entryKey]; exists {
				return fmt.Errorf("proxy pool %q: duplicate entry %q", pool.Name, proxyutil.Redact(pool.Entries[j].URL))
			}
			seenEntries[entryKey] = struct{}{}
		}
	}
	return cfg.validateProxyPoolReferences(names)
}

func (cfg *Config) validateProxyPoolReferences(names map[string]struct{}) error {
	check := func(location, name string) error {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil
		}
		if _, ok := names[strings.ToLower(name)]; !ok {
			return fmt.Errorf("%s: unknown proxy pool %q", location, name)
		}
		return nil
	}
	for i := range cfg.GeminiKey {
		if err := check(fmt.Sprintf("gemini-api-key[%d]", i), cfg.GeminiKey[i].ProxyPool); err != nil {
			return err
		}
	}
	for i := range cfg.InteractionsKey {
		if err := check(fmt.Sprintf("interactions-api-key[%d]", i), cfg.InteractionsKey[i].ProxyPool); err != nil {
			return err
		}
	}
	for i := range cfg.ClaudeKey {
		if err := check(fmt.Sprintf("claude-api-key[%d]", i), cfg.ClaudeKey[i].ProxyPool); err != nil {
			return err
		}
	}
	for i := range cfg.CodexKey {
		if err := check(fmt.Sprintf("codex-api-key[%d]", i), cfg.CodexKey[i].ProxyPool); err != nil {
			return err
		}
	}
	for i := range cfg.XAIKey {
		if err := check(fmt.Sprintf("xai-api-key[%d]", i), cfg.XAIKey[i].ProxyPool); err != nil {
			return err
		}
	}
	for i := range cfg.VertexCompatAPIKey {
		if err := check(fmt.Sprintf("vertex-api-key[%d]", i), cfg.VertexCompatAPIKey[i].ProxyPool); err != nil {
			return err
		}
	}
	for i := range cfg.OpenAICompatibility {
		for j := range cfg.OpenAICompatibility[i].APIKeyEntries {
			if err := check(fmt.Sprintf("openai-compatibility[%d].api-key-entries[%d]", i, j), cfg.OpenAICompatibility[i].APIKeyEntries[j].ProxyPool); err != nil {
				return err
			}
		}
	}
	return nil
}
