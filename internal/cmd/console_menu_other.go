//go:build !windows

package cmd

import (
	"errors"
	"os/exec"
	"runtime"
)

// trayController is a no-op off Windows: the menu keeps working, hide-to-tray
// reports unsupported and defers to the platform init system for background
// runs.
type trayController struct {
	serverURL string
}

func newTrayController(serverURL string) *trayController {
	return &trayController{serverURL: serverURL}
}

func (t *trayController) HideSupported() bool { return false }
func (t *trayController) Start() error        { return errors.New("system tray requires windows") }
func (t *trayController) Stop()               {}
func (t *trayController) HideConsole()        {}
func (t *trayController) WaitHidden()         {}
func (t *trayController) ShowConsole()        {}
func (t *trayController) ExitRequested() bool { return false }

func openInBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
