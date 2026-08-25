// Package panelasset embeds the single-file management panel and installs it
// into the router's static directory.
//
// The real asset is produced by `apps/control-panel` (`npm run
// build:management`) and injected by `scripts/build-local.ps1`, which
// prepends the embeddedMarker comment to the bundle before `go build`. A
// placeholder without the marker is committed so the package always compiles;
// binaries built from it report Available() == false.
package panelasset

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const embeddedMarker = "<!-- ainyrouter-panel-embedded -->"

// ManagementFileName is the filename the router serves at /management.html.
const ManagementFileName = "management.html"

//go:embed panel/management.html
var embeddedPanel []byte

// ErrNotEmbedded reports that this binary was built without the real panel.
var ErrNotEmbedded = errors.New("panel asset is not embedded in this binary; build with scripts/build-local.ps1")

// Available reports whether a real (marker-stamped) panel is embedded.
func Available() bool {
	return bytes.Contains(embeddedPanel, []byte(embeddedMarker))
}

// Install writes the embedded panel into staticDir as management.html and
// returns the written path. It refuses when the binary carries only the
// placeholder, so a stub can never shadow a previously installed real panel.
func Install(staticDir string) (string, error) {
	if !Available() {
		return "", ErrNotEmbedded
	}
	return InstallFrom(staticDir, embeddedPanel)
}

// InstallFrom writes the given panel bytes into staticDir as management.html.
func InstallFrom(staticDir string, data []byte) (string, error) {
	if strings.TrimSpace(staticDir) == "" {
		return "", errors.New("static directory is empty")
	}
	if err := os.MkdirAll(staticDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create static dir: %w", err)
	}
	path := filepath.Join(staticDir, ManagementFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("failed to write %s: %w", path, err)
	}
	return path, nil
}
