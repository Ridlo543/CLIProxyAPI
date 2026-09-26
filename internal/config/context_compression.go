package config

import (
	"fmt"
	"strings"
)
const (
	ContextCompressionOff         = "off"
	ContextCompressionRTK         = "rtk"
	ContextCompressionKompact     = "kompact"
	ContextCompressionTokenSavior = "token_savior"
	ContextCompressionAll         = "all"
)

func (c *ContextCompressionConfig) applyDefaults() {
	switch strings.ToLower(strings.TrimSpace(c.Engine)) {
	case "false", "off", "disabled", "none", "0":
		c.Engine = ContextCompressionOff
	case "tare-structural", "tare", "tare_structural", "rtk_tare", "rtk+tare":
		// TARE is deprecated and removed; map smoothly to RTK
		c.Engine = ContextCompressionRTK
	case "token-savior":
		c.Engine = ContextCompressionTokenSavior
	case "pipeline", "auto", "combined":
		c.Engine = ContextCompressionAll
	}
	if c.Engine == "" {
		if c.Kompact.Enabled {
			c.Engine = ContextCompressionKompact
		} else if c.TokenSavior.Enabled {
			c.Engine = ContextCompressionTokenSavior
		} else {
			c.Engine = ContextCompressionOff
		}
	}
	if c.Kompact.Host == "" {
		c.Kompact.Host = "127.0.0.1"
	}
	if c.Kompact.Port == 0 {
		c.Kompact.Port = 7878
	}
	if c.Kompact.TimeoutMS == 0 {
		c.Kompact.TimeoutMS = 1500
	}
	if c.TokenSavior.Host == "" {
		c.TokenSavior.Host = "127.0.0.1"
	}
	if c.TokenSavior.Port == 0 {
		c.TokenSavior.Port = 8921
	}
	if c.TokenSavior.TimeoutMS == 0 {
		c.TokenSavior.TimeoutMS = 1500
	}
	if c.MinBytes <= 0 {
		c.MinBytes = 500
	}
	if c.RawCapBytes <= 0 {
		c.RawCapBytes = 10 * 1024 * 1024
	}
}

// applyBundledTAREFallback is a no-op kept for signature compatibility.
func (c *ContextCompressionConfig) applyBundledTAREFallback() {}

// Validate ensures engine and size bounds are safe and valid.
func (c ContextCompressionConfig) Validate() error {
	switch c.Engine {
	case ContextCompressionOff, ContextCompressionRTK, ContextCompressionKompact, ContextCompressionTokenSavior, ContextCompressionAll, "":
		// Valid
	default:
		// Sane fallback instead of fatal crash
		return fmt.Errorf("context-compression.engine must be off, rtk, kompact, token_savior, or all")
	}
	if c.MinBytes < 1 || c.MinBytes > 1024*1024 || c.RawCapBytes < c.MinBytes || c.RawCapBytes > 10*1024*1024 {
		return fmt.Errorf("context-compression size bounds are invalid")
	}
	return nil
}
