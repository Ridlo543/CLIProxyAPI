package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	xterm "github.com/charmbracelet/x/term"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
)

// ConsoleMenuOptions carries everything the interactive console menu needs to
// run the server in the background and manage its lifetime.
type ConsoleMenuOptions struct {
	Cfg           *config.Config
	ConfigPath    string
	LocalPassword string
	Host          *pluginhost.Host
	ServerOptions []api.ServerOption
	Version       string
	In            io.Reader // defaults to os.Stdin
	Out           io.Writer // defaults to os.Stdout
}

// RunConsoleMenu starts the proxy in the background and drives the
// 9Router-style terminal menu:
//
//  1. Web UI (Open in Browser)
//  2. Hide to Tray (Background)
//     q) Exit
//
// It blocks until the user quits; server shutdown is always awaited so a
// Ctrl-C style kill never leaves sockets half-open.
func RunConsoleMenu(o ConsoleMenuOptions) error {
	if o.Cfg == nil || o.Cfg.Port == 0 {
		return errors.New("console menu requires a loaded config with a port")
	}
	in := o.In
	if in == nil {
		in = os.Stdin
	}
	out := o.Out
	if out == nil {
		out = os.Stdout
	}

	cancel, done := StartServiceBackgroundWithPluginHost(
		o.Cfg, o.ConfigPath, o.LocalPassword, o.Host, o.ServerOptions...)

	serverURL := fmt.Sprintf("http://localhost:%d", o.Cfg.Port)
	fmt.Fprintf(out, "\nAinyRouter %s\n", menuDisplayVersion(o.Version))
	fmt.Fprintf(out, "  🚀 Server: %s\n", serverURL)
	fmt.Fprintf(out, "========================================\n")

	tray := newTrayController(serverURL)
	trayStartErr := tray.Start()
	if trayStartErr != nil {
		fmt.Fprintf(out, "(system tray unavailable: %v)\n", trayStartErr)
	}
	// Defers run LIFO: tray teardown (best-effort, bounded) first, then the
	// guaranteed server shutdown.
	defer tray.Stop()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			fmt.Fprintln(out, "warning: server shutdown timed out")
		}
	}()

	scanner := bufio.NewScanner(in)
	for {
		printMenu(out)
		if !scanner.Scan() {
			// stdin closed (e.g. piped input exhausted): shut down cleanly.
			fmt.Fprintln(out, "bye")
			return nil
		}
		switch strings.TrimSpace(scanner.Text()) {
		case "1", "open", "web":
			if errOpen := openInBrowser(serverURL); errOpen != nil {
				fmt.Fprintf(out, "failed to open browser: %v\n  open %s manually\n", errOpen, serverURL)
			}
		case "2", "tray", "hide":
			if !tray.HideSupported() {
				fmt.Fprintln(out, "hide-to-tray is only supported on Windows; keep this window open instead")
				continue
			}
			fmt.Fprintln(out, "hidden to tray — use the tray icon (Show Console / Exit)")
			tray.HideConsole()
			tray.WaitHidden()
			tray.ShowConsole()
			fmt.Fprintln(out, "console restored")
			if tray.ExitRequested() {
				fmt.Fprintln(out, "bye")
				return nil
			}
		case "q", "quit", "exit":
			fmt.Fprintln(out, "bye")
			return nil
		default:
			fmt.Fprintln(out, "choose 1, 2, or q")
		}
	}
}

func printMenu(out io.Writer) {
	fmt.Fprint(out, "\n 1) Web UI (Open in Browser)\n 2) Hide to Tray (Background)\n q) Exit\n> ")
}

func menuDisplayVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "dev"
	}
	return v
}

// StdinIsTerminal reports whether standard input is an interactive terminal.
func StdinIsTerminal() bool {
	return xterm.IsTerminal(os.Stdin.Fd())
}

// ShouldUseConsoleMenu decides whether bare `ainyrouter` shows the interactive
// menu instead of plain log output. Explicit flags win; otherwise the menu is
// used only for real terminals, keeping docker/CI/service runs unchanged.
func ShouldUseConsoleMenu(forceMenu, noMenu bool, stdinIsTerminal bool) bool {
	if noMenu {
		return false
	}
	if forceMenu {
		return true
	}
	return stdinIsTerminal && os.Getenv("AINYROUTER_HEADLESS") != "1"
}
