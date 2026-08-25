//go:build !windows

package cmd

import (
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
)

// ErrWindowsServiceUnsupported is returned on non-Windows platforms where
// service management is delegated to the platform init system (systemd,
// launchd) instead of a built-in service wrapper.
var ErrWindowsServiceUnsupported = errors.New("-service is only available on Windows; on this platform use the native service manager (e.g. systemd unit or launchd plist)")

// HandleWindowsService mirrors the Windows-only management entry point.
func HandleWindowsService(action, configPath string) error {
	return ErrWindowsServiceUnsupported
}

// IsWindowsServiceRun reports whether the process runs under a Windows
// service context; always false on non-Windows platforms.
func IsWindowsServiceRun() bool { return false }

// RunWindowsService mirrors the Windows-only supervised run entry point.
func RunWindowsService(cfg *config.Config, configFilePath, password string, host *pluginhost.Host, serverOptions ...api.ServerOption) error {
	return ErrWindowsServiceUnsupported
}
