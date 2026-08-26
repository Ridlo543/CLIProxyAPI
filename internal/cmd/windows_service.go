//go:build windows

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsServiceName = "CLIProxyAPI"
	startWaitHintMS    = 30_000
	stopWaitHintMS     = 45_000
	installTimeout     = 30 * time.Second
	stopTimeout        = 45 * time.Second
)

// errEventLogSourceExists is ERROR_SERVICE_EXISTS_ALREADY (1073).
var errEventLogSourceExists = syscall.Errno(1073)

// ErrNotUnderServiceControl is returned when -service run is executed outside
// the Service Control Manager context.
var ErrNotUnderServiceControl = errors.New("-service run must be launched by the Windows Service Control Manager; use `-service install` and then start the CLIProxyAPI service")

// HandleWindowsService executes one of the management actions for the
// Windows service wrapper. configPath is baked into the service command line
// verbatim, so it must already be absolute when action is "install".
//
// Actions: install | remove | start | stop | status. "run" must go through
// RunWindowsService after full application init instead.
func HandleWindowsService(action, configPath string) error {
	switch action {
	case "install", "remove", "start", "stop", "status":
	default:
		return fmt.Errorf("unknown -service action %q (use install|remove|start|stop|status)", action)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot connect to the Service Control Manager (administrator rights required): %w", err)
	}
	defer m.Disconnect()

	s, openErr := m.OpenService(windowsServiceName)
	switch action {
	case "status":
		if openErr != nil {
			fmt.Println("not installed")
			return nil
		}
		defer s.Close()
		st, errQuery := s.Query()
		if errQuery != nil {
			return errQuery
		}
		fmt.Println(serviceStateString(st.State))
		return nil
	case "remove":
		if openErr != nil {
			return fmt.Errorf("service %q is not installed", windowsServiceName)
		}
		defer s.Close()
		if err := s.Delete(); err != nil {
			return fmt.Errorf("failed to delete service: %w", err)
		}
		fmt.Printf("service %q removed\n", windowsServiceName)
		return nil
	case "start":
		if openErr != nil {
			return fmt.Errorf("service %q is not installed (run -service install first)", windowsServiceName)
		}
		defer s.Close()
		if err := s.Start(); err != nil {
			return fmt.Errorf("failed to start service: %w", err)
		}
		if !waitForState(s, svc.Running) {
			return errors.New("timed out waiting for the service to reach RUNNING")
		}
		fmt.Printf("service %q is running\n", windowsServiceName)
		return nil
	case "stop":
		if openErr != nil {
			return fmt.Errorf("service %q is not installed", windowsServiceName)
		}
		defer s.Close()
		return stopService(s)
	case "install":
		if openErr == nil {
			s.Close()
			return fmt.Errorf("service %q is already installed", windowsServiceName)
		}
		return installService(m, exeAbsPath(), configAbsPath(configPath))
	}
	return fmt.Errorf("unknown -service action %q", action)
}

func installService(m *mgr.Mgr, exePath, configPath string) error {
	s, err := m.CreateService(windowsServiceName, exePath, mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: "CLI Proxy API",
		Description: "CLIProxyAPI proxy server providing OpenAI/Gemini/Claude compatible APIs.",
	},
		"--config", configPath,
		"-service", "run",
	)
	if err != nil {
		return fmt.Errorf("failed to create service: %w", err)
	}
	defer s.Close()

	recovery := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 2 * time.Minute},
	}
	if err := s.SetRecoveryActions(recovery, 86400); err != nil {
		log.WithError(err).Warn("failed to configure failure recovery actions")
	}
	// Best-effort: an existing event log registration from a previous install
	// is not an error.
	for _, kind := range []uint32{eventlog.Error, eventlog.Warning, eventlog.Info} {
		if err := eventlog.InstallAsEventCreate(exeAbsPath(), kind); err != nil {
			var errno syscall.Errno
			if errors.As(err, &errno) && errno == errEventLogSourceExists {
				break
			}
			log.WithError(err).Warn("failed to register event log source")
		}
	}
	fmt.Printf("service %q installed (config: %s); start it with: net start %s\n",
		windowsServiceName, configPath, windowsServiceName)
	return nil
}

func stopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		fmt.Println("service is already stopped")
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("failed to send stop control: %w", err)
	}
	if !waitForState(s, svc.Stopped) {
		return errors.New("timed out waiting for the service to reach STOPPED")
	}
	fmt.Println("service stopped")
	return nil
}

func waitForState(s *mgr.Service, want svc.State) bool {
	deadline := time.Now().Add(installTimeout)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil {
			return false
		}
		if st.State == want {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func serviceStateString(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "resuming"
	case svc.PausePending:
		return "pausing"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("unknown(%d)", state)
}

// IsWindowsServiceRun reports whether this process was launched by the
// Service Control Manager in response to a `-service run` command line.
func IsWindowsServiceRun() bool {
	inService, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return inService
}

func exeAbsPath() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return exe
	}
	return abs
}

func configAbsPath(configPath string) string {
	if configPath == "" {
		wd, _ := os.Getwd()
		return filepath.Join(wd, "config.yaml")
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return configPath
	}
	return abs
}

// RunWindowsService runs the proxy under Service Control Manager supervision.
// It must only be called with a fully initialized configuration, mirroring
// the normal StartServiceWithPluginHost entry point.
func RunWindowsService(cfg *config.Config, configFilePath, password string, host *pluginhost.Host, serverOptions ...api.ServerOption) error {
	if !IsWindowsServiceRun() {
		return ErrNotUnderServiceControl
	}
	// Services start with C:\Windows\System32 as working directory. Anchor on
	// the executable directory so relative auth-dir/log paths resolve sanely.
	if dir := filepath.Dir(exeAbsPath()); dir != "" {
		if err := os.Chdir(dir); err != nil {
			log.WithError(err).Warn("failed to change working directory to executable directory")
		}
	}
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configFilePath).
		WithLocalManagementPassword(password)
	if host != nil {
		builder = builder.WithPluginHost(host)
	}
	if len(serverOptions) > 0 {
		builder = builder.WithServerOptions(serverOptions...)
	}
	service, err := builder.Build()
	if err != nil {
		return fmt.Errorf("failed to build proxy service: %w", err)
	}
	h := &windowsServiceHandler{buildAndRun: func(ctx context.Context) error {
		runErr := service.Run(ctx)
		if errors.Is(runErr, context.Canceled) {
			return nil
		}
		return runErr
	}}
	return svc.Run(windowsServiceName, h)
}

type windowsServiceHandler struct {
	buildAndRun func(ctx context.Context) error
}

// Execute implements svc.Handler.
func (h *windowsServiceHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending, WaitHint: startWaitHintMS}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := h.buildAndRun(ctx); err != nil {
			log.WithError(err).Error("proxy service exited with error")
		}
	}()

	// Init is done as soon as the run goroutine is launched; report RUNNING
	// so the SCM marks the service healthy and enables stop controls.
	status <- svc.Status{State: svc.Running, Accepts: accepted}

	exitCode := uint32(0)
loop:
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: stopWaitHintMS}
				cancel()
				break loop
			default:
				log.Warnf("ignoring unexpected service control request: %d", c.Cmd)
			}
		case <-done:
			break loop
		}
	}
	select {
	case <-done:
	case <-time.After(stopTimeout):
		log.Error("proxy service did not shut down in time")
		exitCode = 1
	}
	cancel()
	status <- svc.Status{State: svc.Stopped}
	return false, exitCode
}
