package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	ContextCompressionOff     = "off"
	ContextCompressionRTK     = "rtk"
	ContextCompressionTARE    = "tare_structural"
	ContextCompressionRTKTARE = "rtk_tare"
)

var sha256Pattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

const (
	bundledTAREBinaryEnv     = "AINYROUTER_TARE_BINARY"
	bundledTARESHA256Env     = "AINYROUTER_TARE_SHA256"
	bundledTAREVersionEnv    = "AINYROUTER_TARE_VERSION"
	bundledTAREManifestIDEnv = "AINYROUTER_TARE_MANIFEST_ID"
)

func (c *ContextCompressionConfig) applyDefaults() {
	switch c.Engine {
	case "tare-structural":
		c.Engine = ContextCompressionTARE
	case "rtk+tare":
		c.Engine = ContextCompressionRTKTARE
	}
	if c.Engine == "" {
		c.Engine = ContextCompressionOff
	}
	if c.MinBytes == 0 {
		c.MinBytes = 500
	}
	if c.RawCapBytes == 0 {
		c.RawCapBytes = 10 * 1024 * 1024
	}
	if c.TARE.ProcessTimeoutMS == 0 {
		c.TARE.ProcessTimeoutMS = 3000
	}
	if c.TARE.QueueTimeoutMS == 0 {
		c.TARE.QueueTimeoutMS = 500
	}
	if c.TARE.InputLimitBytes == 0 {
		c.TARE.InputLimitBytes = 1024*1024 + 1024
	}
	if c.TARE.StdoutLimitBytes == 0 {
		c.TARE.StdoutLimitBytes = 1024 * 1024
	}
	if c.TARE.StderrLimitBytes == 0 {
		c.TARE.StderrLimitBytes = 64 * 1024
	}
	if c.TARE.GlobalConcurrency == 0 {
		c.TARE.GlobalConcurrency = 1
	}
	if c.TARE.CacheEntries == 0 {
		c.TARE.CacheEntries = 128
	}
	if c.TARE.CacheBytes == 0 {
		c.TARE.CacheBytes = 16 * 1024 * 1024
	}
}

// applyBundledTAREFallback uses image-provided identity only when the canonical
// TARE engine has no explicit identity fields. A partial explicit identity is
// deliberately left untouched so validation rejects it.
func (c *ContextCompressionConfig) applyBundledTAREFallback() {
	if c.Engine != ContextCompressionTARE && c.Engine != ContextCompressionRTKTARE {
		return
	}
	t := &c.TARE
	explicit := t.BinaryPath != "" || t.SHA256 != "" || len(t.AllowedVersions) != 0 || t.ManifestID != ""
	if explicit {
		return
	}
	t.BinaryPath = strings.TrimSpace(os.Getenv(bundledTAREBinaryEnv))
	t.SHA256 = strings.TrimSpace(os.Getenv(bundledTARESHA256Env))
	version := strings.TrimSpace(os.Getenv(bundledTAREVersionEnv))
	if version != "" {
		t.AllowedVersions = []string{version}
	}
	t.ManifestID = strings.TrimSpace(os.Getenv(bundledTAREManifestIDEnv))
}

// Validate rejects unsafe or unbounded compression configuration at load time.
func (c ContextCompressionConfig) Validate() error {
	if c.Engine != ContextCompressionOff && c.Engine != ContextCompressionRTK && c.Engine != ContextCompressionTARE && c.Engine != ContextCompressionRTKTARE {
		return fmt.Errorf("context-compression.engine must be off, rtk, tare_structural, or rtk_tare")
	}
	if c.MinBytes < 1 || c.MinBytes > 1024*1024 || c.RawCapBytes < c.MinBytes || c.RawCapBytes > 10*1024*1024 {
		return fmt.Errorf("context-compression size bounds are invalid")
	}
	t := c.TARE
	if t.ProcessTimeoutMS < 1 || t.ProcessTimeoutMS > 15000 || t.QueueTimeoutMS < 1 || t.QueueTimeoutMS > 5000 ||
		t.InputLimitBytes < 1024 || t.InputLimitBytes > 1024*1024+1024 || t.StdoutLimitBytes < 256 || t.StdoutLimitBytes > 1024*1024 ||
		t.StderrLimitBytes < 256 || t.StderrLimitBytes > 64*1024 || t.GlobalConcurrency != 1 ||
		t.CacheEntries < 1 || t.CacheEntries > 128 || t.CacheBytes < 1024 || t.CacheBytes > 16*1024*1024 {
		return fmt.Errorf("context-compression.tare-structural bounds are invalid")
	}
	if t.SHA256 != "" && !sha256Pattern.MatchString(t.SHA256) {
		return fmt.Errorf("context-compression TARE checksum is invalid")
	}
	if len(t.AllowedVersions) > 8 {
		return fmt.Errorf("context-compression TARE version allowlist is too large")
	}
	for _, version := range t.AllowedVersions {
		if version == "" || len(version) > 64 {
			return fmt.Errorf("context-compression TARE version allowlist is invalid")
		}
	}
	if c.Engine == ContextCompressionTARE || c.Engine == ContextCompressionRTKTARE {
		if !filepath.IsAbs(t.BinaryPath) || !sha256Pattern.MatchString(t.SHA256) || len(t.AllowedVersions) == 0 || t.ManifestID == "" || len(t.ManifestID) > 128 {
			return fmt.Errorf("context-compression tare-structural and rtk_tare require an absolute binary path, checksum, version allowlist, and manifest id")
		}
	}
	return nil
}
