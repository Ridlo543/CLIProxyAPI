package cmd

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolveConfigPath implements the config lookup chain for the CLI:
//
//  1. an explicit --config value (anything other than the default name) wins
//  2. ./config.yaml next to the working directory (repo/dev flow)
//  3. the per-user install location created by scripts\install-user.ps1
//     (%LOCALAPPDATA%\AinyRouter\config.yaml via os.UserConfigDir)
//
// This is what makes bare `ainyrouter` work from any directory.
func ResolveConfigPath(configPath string) string {
	explicit := strings.TrimSpace(configPath) != ""
	if explicit && filepath.Base(filepath.Clean(configPath)) != "config.yaml" {
		return configPath // user pointed at a specific file
	}
	if explicit {
		if _, err := os.Stat(configPath); err == nil {
			return configPath
		}
	}
	// Per-user install locations, most specific first:
	//   %LOCALAPPDATA%\AinyRouter\config.yaml  (scripts\install-user.ps1)
	//   <os.UserConfigDir()>\AinyRouter\config.yaml
	var candidates []string
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		candidates = append(candidates, filepath.Join(la, "AinyRouter", "config.yaml"))
	}
	if cfgDir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(cfgDir, "AinyRouter", "config.yaml"))
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return configPath // surface the original "not found" error upstream
}
