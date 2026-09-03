// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ContextCompression optionally compresses eligible historical tool outputs inline.
	// The original client payload remains available to model routers and request telemetry.
	ContextCompression ContextCompressionConfig `yaml:"context-compression" json:"context-compression"`
	// ProxyPools defines reusable named outbound proxy selections.
	ProxyPools []ProxyPool `yaml:"proxy-pools,omitempty" json:"-"`

	// ModelGroups defines client-visible models backed by ordered fallback targets.
	ModelGroups []ModelGroup `yaml:"model-groups,omitempty" json:"model-groups,omitempty"`

	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	//   - "passthrough": do not modify the tool list on non-images endpoints — keep image_generation if the client
	//     sent it and do not inject it otherwise; on /v1/images/generations and /v1/images/edits behave like "chat".
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base (mainline) model used by the legacy hosted
	// image_generation tool path when a Codex image request is not proxied directly
	// through the Image API.
	//
	// The value must start with "gpt-" (case-insensitive). If empty or invalid, the
	// default base model ("gpt-5.4-mini") is used.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// CodexOptimizeMultiAgentV2 mirrors the provider-wide runtime setting for API handlers.
	CodexOptimizeMultiAgentV2 bool `yaml:"-" json:"-"`

	// CodexOrphanDelegationCompatibility mirrors the provider-wide runtime setting for API handlers.
	CodexOrphanDelegationCompatibility bool `yaml:"-" json:"-"`

	// ClaudeCode configures Claude Code compatibility behavior.
	ClaudeCode ClaudeCodeConfig `yaml:"claude-code" json:"claude-code"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// APIKeyPolicies attaches additive per-key model/provider/token restrictions.
	// An empty list preserves the legacy behaviour where every key may call
	// every model and provider without token budgets.
	APIKeyPolicies []APIKeyPolicy `yaml:"api-key-policies,omitempty" json:"-"`
	KeyPolicies    []APIKeyPolicy `yaml:"key-policies,omitempty" json:"-"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// ContextCompressionConfig configures request compression engines (RTK, TARE, Kompact, Token Savior).
// TARE executable identity fields are intentionally excluded from JSON management surfaces.
type ContextCompressionConfig struct {
	Engine      string               `yaml:"engine" json:"engine"`
	MinBytes    int                  `yaml:"min-bytes,omitempty" json:"min-bytes,omitempty"`
	RawCapBytes int                  `yaml:"raw-cap-bytes,omitempty" json:"raw-cap-bytes,omitempty"`
	TARE        TAREStructuralConfig `yaml:"tare-structural,omitempty" json:"-"`
	Kompact     KompactConfig        `yaml:"kompact,omitempty" json:"kompact,omitempty"`
	TokenSavior TokenSaviorConfig    `yaml:"token-savior,omitempty" json:"token-savior,omitempty"`
}

// KompactConfig configures connection to the external Kompact context optimization proxy.
type KompactConfig struct {
	Enabled           bool   `yaml:"enabled" json:"enabled"`
	Host              string `yaml:"host,omitempty" json:"host,omitempty"`
	Port              int    `yaml:"port,omitempty" json:"port,omitempty"`
	TimeoutMS         int    `yaml:"timeout-ms,omitempty" json:"timeout-ms,omitempty"`
	Toon              bool   `yaml:"toon,omitempty" json:"toon,omitempty"`
	ObservationMasker bool   `yaml:"observation-masker,omitempty" json:"observation-masker,omitempty"`
	CacheAligner      bool   `yaml:"cache-aligner,omitempty" json:"cache-aligner,omitempty"`
	JSONCrusher       bool   `yaml:"json-crusher,omitempty" json:"json-crusher,omitempty"`
	CodeCompressor    bool   `yaml:"code-compressor,omitempty" json:"code-compressor,omitempty"`
	LogCompressor     bool   `yaml:"log-compressor,omitempty" json:"log-compressor,omitempty"`
	HTMLStripper      bool   `yaml:"html-stripper,omitempty" json:"html-stripper,omitempty"`
	ContentCompressor bool   `yaml:"content-compressor,omitempty" json:"content-compressor,omitempty"`
}

// TokenSaviorConfig configures connection to the external Token Savior daemon.
type TokenSaviorConfig struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	Host        string `yaml:"host,omitempty" json:"host,omitempty"`
	Port        int    `yaml:"port,omitempty" json:"port,omitempty"`
	TimeoutMS   int    `yaml:"timeout-ms,omitempty" json:"timeout-ms,omitempty"`
	Profile     string `yaml:"profile,omitempty" json:"profile,omitempty"`
	BashCompact bool   `yaml:"bash-compact,omitempty" json:"bash-compact,omitempty"`
	BashRewrite bool   `yaml:"bash-rewrite,omitempty" json:"bash-rewrite,omitempty"`
}

// TAREStructuralConfig contains only bounded process and verified-identity settings.
type TAREStructuralConfig struct {
	BinaryPath        string   `yaml:"binary-path,omitempty" json:"-"`
	SHA256            string   `yaml:"sha256,omitempty" json:"-"`
	AllowedVersions   []string `yaml:"allowed-versions,omitempty" json:"-"`
	ManifestID        string   `yaml:"manifest-id,omitempty" json:"-"`
	ProcessTimeoutMS  int      `yaml:"process-timeout-ms,omitempty" json:"-"`
	QueueTimeoutMS    int      `yaml:"queue-timeout-ms,omitempty" json:"-"`
	InputLimitBytes   int      `yaml:"input-limit-bytes,omitempty" json:"-"`
	StdoutLimitBytes  int      `yaml:"stdout-limit-bytes,omitempty" json:"-"`
	StderrLimitBytes  int      `yaml:"stderr-limit-bytes,omitempty" json:"-"`
	GlobalConcurrency int      `yaml:"global-concurrency,omitempty" json:"-"`
	CacheEntries      int      `yaml:"cache-entries,omitempty" json:"-"`
	CacheBytes        int      `yaml:"cache-bytes,omitempty" json:"-"`
}

// ClaudeCodeConfig configures Claude Code compatibility behavior.
type ClaudeCodeConfig struct {
	// DisableCloakingModelList disables model ID cloaking in Anthropic model list responses.
	DisableCloakingModelList bool `yaml:"disable-cloaking-model-list" json:"disable-cloaking-model-list"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n")
	// or WebSocket Ping control frames.
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
