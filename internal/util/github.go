package util

import (
	"os"
	"strings"
)

// ResolveGitHubToken returns the configured GitHub API token in priority order:
// 1. GITHUB_TOKEN
// 2. github_token
// 3. GITSTORE_GIT_TOKEN (only if GITSTORE_GIT_URL points to github.com)
func ResolveGitHubToken() string {
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token
	}
	if token := strings.TrimSpace(os.Getenv("github_token")); token != "" {
		return token
	}
	if strings.Contains(strings.ToLower(os.Getenv("GITSTORE_GIT_URL")), "github.com") {
		if token := strings.TrimSpace(os.Getenv("GITSTORE_GIT_TOKEN")); token != "" {
			return token
		}
	}
	return ""
}
