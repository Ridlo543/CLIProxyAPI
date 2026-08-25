package cmd

import (
	"errors"
	"flag"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/panelasset"
)

// HandlePanelInstall writes the embedded management panel into the router's
// static directory so GET /management.html serves it.
//
// Wired to `-panel-install` in cmd/server/main.go. It needs only the config
// path (the static dir is derived from it), so it runs before any
// configuration loading, mirroring -service handling.
func HandlePanelInstall(fs *flag.FlagSet, configPath string) error {
	staticDir := managementasset.StaticDir(configPath)
	path, err := panelasset.Install(staticDir)
	if errors.Is(err, panelasset.ErrNotEmbedded) {
		return fmt.Errorf("%w\n  hint: run scripts/build-local.ps1 first, or serve apps/control-panel manually", err)
	}
	if err != nil {
		return err
	}
	fmt.Printf("panel installed: %s\n", path)
	fmt.Println("served at: /management.html")
	fmt.Println("recommended config under remote-management:")
	fmt.Println("  disable-auto-update-panel: true   # keeps this file from being replaced")
	return nil
}
