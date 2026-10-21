package pluginstore

import (
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/githubauth"
)

// TestMain isolates the suite from the developer's ambient GitHub credentials.
// A token in GITHUB_TOKEN (or a configured shared token) changes download routing
// between the API asset URL and the browser URL, so the suite must not inherit
// one by accident.
func TestMain(m *testing.M) {
	names := []string{"GITHUB_TOKEN", "github_token", "GITSTORE_GIT_TOKEN"}
	restore := make(map[string]string, len(names))
	for _, name := range names {
		restore[name] = os.Getenv(name)
		if err := os.Setenv(name, ""); err != nil {
			panic(err)
		}
	}
	previousToken := githubauth.ResolveToken()
	githubauth.SetToken("")

	code := m.Run()

	for name, value := range restore {
		_ = os.Setenv(name, value)
	}
	githubauth.SetToken(previousToken)
	os.Exit(code)
}
