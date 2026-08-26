//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
)

// trayController manages the Windows notification-area icon and the console
// window visibility for the interactive menu.
type trayController struct {
	serverURL string

	mu        sync.Mutex
	hidden    bool
	exitReq   bool
	unhide    chan struct{} // closed when the user asks to leave hidden mode
	stopped   chan struct{}
	startOnce sync.Once
}

func newTrayController(serverURL string) *trayController {
	return &trayController{
		serverURL: serverURL,
		unhide:    make(chan struct{}),
		stopped:   make(chan struct{}),
	}
}

func (t *trayController) HideSupported() bool { return true }

// Start launches the systray icon. systray requires a dedicated locked OS
// thread for its message loop; failures are non-fatal because the menu keeps
// working without the icon.
func (t *trayController) Start() error {
	t.startOnce.Do(func() {
		go func() {
			runtime.LockOSThread()
			defer close(t.stopped)
			systray.Run(t.onReady, t.onExit)
		}()
	})
	return nil
}

// Stop asks the systray loop to exit and waits briefly. It must never block
// the caller indefinitely — the console menu shuts the server down first.
func (t *trayController) Stop() {
	systray.Quit()
	select {
	case <-t.stopped:
	case <-time.After(2 * time.Second):
	}
}

func (t *trayController) onReady() {
	systray.SetIcon(generateTrayIconICO())
	systray.SetTitle("AinyRouter")
	systray.SetTooltip("AinyRouter — running in background")

	mOpen := systray.AddMenuItem("Open Web UI", "Open the control panel in your browser")
	mShow := systray.AddMenuItem("Show Console", "Restore the console window")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("Exit", "Stop AinyRouter and quit")

	mOpen.Enable()
	mShow.Enable()
	mExit.Enable()

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				if errOpen := openInBrowser(t.serverURL); errOpen != nil {
					fmt.Fprintf(os.Stderr, "open browser failed: %v\n", errOpen)
				}
			case <-mShow.ClickedCh:
				t.ShowConsole()
			case <-mExit.ClickedCh:
				t.requestExit()
				systray.Quit()
				return
			case <-t.stopped:
				return
			}
		}
	}()
}

func (t *trayController) onExit() {}

// HideConsole hides the current console window. Safe to call repeatedly.
func (t *trayController) HideConsole() {
	t.mu.Lock()
	t.hidden = true
	t.mu.Unlock()
	if hwnd := currentConsoleWindow(); hwnd != 0 {
		_ = showWindow(hwnd, swHide)
	}
}

// ShowConsole restores the console window and leaves hidden mode.
func (t *trayController) ShowConsole() {
	t.mu.Lock()
	wasHidden := t.hidden
	t.hidden = false
	t.mu.Unlock()
	if hwnd := currentConsoleWindow(); hwnd != 0 && wasHidden {
		_ = showWindow(hwnd, swShow)
	}
	t.leaveHiddenMode()
}

func (t *trayController) WaitHidden() {
	<-t.hiddenDone()
}

func (t *trayController) ExitRequested() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exitReq
}

func (t *trayController) requestExit() {
	t.mu.Lock()
	t.exitReq = true
	t.mu.Unlock()
	t.leaveHiddenMode()
}

func (t *trayController) leaveHiddenMode() {
	t.mu.Lock()
	ch := t.unhide
	alreadyClosed := false
	select {
	case <-ch:
		alreadyClosed = true
	default:
	}
	if !alreadyClosed {
		close(ch)
	}
	t.mu.Unlock()
}

func (t *trayController) hiddenDone() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unhide
}

const (
	swHide = 0
	swShow = 5
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
)

func currentConsoleWindow() uintptr {
	hwnd, _, _ := procGetConsoleWindow.Call()
	return hwnd
}

func showWindow(hwnd uintptr, cmd int32) bool {
	ret, _, _ := procShowWindow.Call(hwnd, uintptr(cmd))
	return ret != 0
}

var _ = errors.New // placeholder to keep errors imported if helpers change

func openInBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
