package auth

// CanonicalCredentialMetadataKey returns the canonical snake_case name for
// credential metadata keys that previously also accepted config-style aliases or camelCase.
func CanonicalCredentialMetadataKey(key string) string {
	switch key {
	case "api-key", "apiKey":
		return "api_key"
	case "base-url", "baseUrl":
		return "base_url"
	case "disable-cooling", "disableCooling":
		return "disable_cooling"
	case "excluded-models", "excludedModels":
		return "excluded_models"
	case "fingerprint-profile", "fingerprintProfile":
		return "fingerprint_profile"
	case "model-aliases", "modelAliases":
		return "model_aliases"
	case "proxy-url", "proxyUrl":
		return "proxy_url"
	case "request-retry", "requestRetry":
		return "request_retry"
	case "request-scoped-errors", "requestScopedErrors":
		return "request_scoped_errors"
	case "tool-prefix-disabled", "toolPrefixDisabled":
		return "tool_prefix_disabled"
	case "accessToken":
		return "access_token"
	case "refreshToken":
		return "refresh_token"
	case "idToken":
		return "id_token"
	case "expiresAt":
		return "expired"
	case "expiresIn":
		return "expires_in"
	case "provider":
		return "type"
	default:
		return key
	}
}

// NormalizeCredentialMetadata rewrites recognized legacy keys to their
// canonical snake_case names. An explicitly present canonical value wins.
func NormalizeCredentialMetadata(metadata map[string]any) {
	for key, value := range metadata {
		canonical := CanonicalCredentialMetadataKey(key)
		if canonical == key {
			continue
		}
		if _, exists := metadata[canonical]; !exists {
			metadata[canonical] = value
		}
		delete(metadata, key)
	}
}
