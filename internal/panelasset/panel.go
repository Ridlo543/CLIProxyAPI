// Package panelasset embeds the built single-file management panel (plus its
// public assets) and installs it into the router's static directory.
//
// scripts/build-local.ps1 copies apps/control-panel/dist-management/* into
// the embedded panel/ directory and prepends embeddedMarker to
// management.html. A placeholder without the marker is committed so the
// package always compiles; binaries built only from it report
// Available() == false.
package panelasset

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const embeddedMarker = "<!-- ainyrouter-panel-embedded -->"

// ManagementFileName is the filename the router serves at /management.html.
const ManagementFileName = "management.html"

//go:embed all:panel
var embeddedPanel embed.FS

var ErrNotEmbedded = errors.New("panel asset is not embedded in this binary; build with scripts/build-local.ps1")

// Available reports whether a real (marker-stamped) management.html exists in
// the embedded panel directory.
func Available() bool {
	data, err := embeddedPanel.ReadFile("panel/" + ManagementFileName)
	return err == nil && bytes.Contains(data, []byte(embeddedMarker))
}

// Install writes every embedded panel file into staticDir, preserving
// relative paths (management.html, favicon.svg, providers/...). It refuses
// when only the placeholder is present so a stub can never shadow a real
// installation. Returns the management.html path.
func Install(staticDir string) (string, error) {
	if !Available() {
		return "", ErrNotEmbedded
	}
	if strings.TrimSpace(staticDir) == "" {
		return "", errors.New("static directory is empty")
	}
	root := "panel"
	count := 0
	errWalk := fs.WalkDir(embeddedPanel, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, errRead := embeddedPanel.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		target := filepath.Join(staticDir, filepath.FromSlash(strings.TrimPrefix(path, root+"/")))
		if errMk := os.MkdirAll(filepath.Dir(target), 0o755); errMk != nil {
			return fmt.Errorf("failed to create dir for %s: %w", target, errMk)
		}
		if errWrite := os.WriteFile(target, data, 0o644); errWrite != nil {
			return fmt.Errorf("failed to write %s: %w", target, errWrite)
		}
		count++
		return nil
	})
	if errWalk != nil {
		return "", fmt.Errorf("failed to install panel: %w", errWalk)
	}
	if count == 0 {
		return "", errors.New("embedded panel directory was empty")
	}
	return filepath.Join(staticDir, ManagementFileName), nil
}

// InstallFrom writes just the given bytes as management.html (kept for tests
// and simple callers).
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
