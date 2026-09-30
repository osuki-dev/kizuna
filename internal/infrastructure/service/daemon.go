package service

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kardianos/service"
)

// DaemonConfig holds service configuration
type DaemonConfig struct {
	Name        string
	DisplayName string
	Description string
	Arguments   []string
	UserService bool
}

// Program is the service runner implementation
type Program struct {
	exit    chan struct{}
	runFunc func()
}

func (p *Program) Start(s service.Service) error {
	p.exit = make(chan struct{})
	go p.run()
	return nil
}

func (p *Program) run() {
	if p.runFunc != nil {
		p.runFunc()
	}
}

func (p *Program) Stop(s service.Service) error {
	close(p.exit)
	return nil
}

// Manager wraps kardianos/service
type Manager struct {
	svc    service.Service
	config DaemonConfig
}

// NewManager creates a daemon service manager
func NewManager(cfg DaemonConfig, runFunc func()) (*Manager, error) {
	execPath, err := os.Executable()
	if err != nil {
		return nil, err
	}

	targetPath := execPath
	home, _ := os.UserHomeDir()
	if cfg.UserService && home != "" {
		preferredDir := filepath.Join(home, ".local", "bin")
		preferredPath := filepath.Join(preferredDir, "kizuna")
		if execPath != preferredPath {
			_ = os.MkdirAll(preferredDir, 0755)
			if err := copyBinary(execPath, preferredPath); err == nil {
				targetPath = preferredPath
			}
		}
	} else if !cfg.UserService && os.Geteuid() == 0 {
		preferredPath := "/usr/local/bin/kizuna"
		if execPath != preferredPath {
			if err := copyBinary(execPath, preferredPath); err == nil {
				targetPath = preferredPath
			}
		}
	}

	opts := service.KeyValue{
		"Restart":    "always",
		"RestartSec": 5,
		"KeepAlive":  true,
	}
	if cfg.UserService {
		opts["UserService"] = true
	}

	svcConfig := &service.Config{
		Name:        cfg.Name,
		DisplayName: cfg.DisplayName,
		Description: cfg.Description,
		Executable:  targetPath,
		Arguments:   cfg.Arguments,
		Option:      opts,
	}

	prg := &Program{runFunc: runFunc}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		return nil, err
	}

	return &Manager{svc: s, config: cfg}, nil
}

func copyBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	_ = os.Remove(dst)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return os.Chmod(dst, 0755)
}

// Restart stops and starts the background service
func (m *Manager) Restart() error {
	_ = m.svc.Stop()
	return m.svc.Start()
}

func (m *Manager) preInstallDirs() {
	if m.config.UserService {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			switch runtime.GOOS {
			case "darwin":
				_ = os.MkdirAll(filepath.Join(home, "Library", "LaunchAgents"), 0755)
			case "linux":
				_ = os.MkdirAll(filepath.Join(home, ".config", "systemd", "user"), 0755)
			}
		}
	}
}

// Install registers the service with the OS init system (systemd/launchd/windows service)
func (m *Manager) Install() error {
	m.preInstallDirs()
	return m.svc.Install()
}

// Uninstall removes the service from the OS init system
func (m *Manager) Uninstall() error {
	return m.svc.Uninstall()
}

// Start launches the background service
func (m *Manager) Start() error {
	return m.svc.Start()
}

// Stop stops the background service
func (m *Manager) Stop() error {
	return m.svc.Stop()
}

// Run executes the service in the foreground or as daemon
func (m *Manager) Run() error {
	return m.svc.Run()
}

// ServiceStatus represents OS daemon status
type ServiceStatus string

const (
	StatusRunning ServiceStatus = "running"
	StatusStopped ServiceStatus = "stopped"
	StatusUnknown ServiceStatus = "unknown"
)

// GetStatus returns the human-readable service status
func (m *Manager) GetStatus() (ServiceStatus, error) {
	status, err := m.svc.Status()
	if err == nil && status == service.StatusRunning {
		return StatusRunning, nil
	}

	// On Darwin, kardianos/service queries launchctl list <name>, which cannot see services in other domains
	// (e.g. system domain when called by non-root user). Check launchctl print and process table.
	if runtime.GOOS == "darwin" {
		if isDarwinServiceRunning(m.config.Name) {
			return StatusRunning, nil
		}
	}

	// On Linux, check systemctl and process table
	if runtime.GOOS == "linux" {
		if isLinuxServiceRunning(m.config.Name) {
			return StatusRunning, nil
		}
	}

	if err != nil {
		if IsInstalled(m.config.Name) {
			return StatusStopped, nil
		}
		return StatusUnknown, fmt.Errorf("unable to check status: %w", err)
	}

	switch status {
	case service.StatusRunning:
		return StatusRunning, nil
	case service.StatusStopped:
		return StatusStopped, nil
	default:
		return StatusUnknown, nil
	}
}

// Status returns current raw service status
func (m *Manager) Status() (service.Status, error) {
	status, err := m.svc.Status()
	if err != nil {
		if runtime.GOOS == "darwin" && isDarwinServiceRunning(m.config.Name) {
			return service.StatusRunning, nil
		}
		if runtime.GOOS == "linux" && isLinuxServiceRunning(m.config.Name) {
			return service.StatusRunning, nil
		}
		return service.StatusUnknown, fmt.Errorf("unable to check status: %w", err)
	}
	return status, nil
}

// DetectUserService detects whether the service is registered as a user service or system service.
// Returns true if installed as user service or if non-root and not installed as system service.
func DetectUserService(serviceName string) bool {
	if os.Geteuid() == 0 {
		return false
	}
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err == nil {
			userPlist := filepath.Join(home, "Library/LaunchAgents", serviceName+".plist")
			if _, err := os.Stat(userPlist); err == nil {
				return true
			}
		}
		systemPlist := filepath.Join("/Library/LaunchDaemons", serviceName+".plist")
		if _, err := os.Stat(systemPlist); err == nil {
			return false
		}
		return true
	case "linux":
		home, err := os.UserHomeDir()
		if err == nil {
			userService := filepath.Join(home, ".config/systemd/user", serviceName+".service")
			if _, err := os.Stat(userService); err == nil {
				return true
			}
		}
		systemService := filepath.Join("/etc/systemd/system", serviceName+".service")
		if _, err := os.Stat(systemService); err == nil {
			return false
		}
		return true
	default:
		return false
	}
}

// IsSystemInstalled checks if the service is installed in system daemon directory.
func IsSystemInstalled(serviceName string) bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := os.Stat(filepath.Join("/Library/LaunchDaemons", serviceName+".plist"))
		return err == nil
	case "linux":
		_, err := os.Stat(filepath.Join("/etc/systemd/system", serviceName+".service"))
		return err == nil
	default:
		return false
	}
}

// IsInstalled checks if either user or system service file exists.
func IsInstalled(serviceName string) bool {
	if IsSystemInstalled(serviceName) {
		return true
	}
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err == nil {
			_, err := os.Stat(filepath.Join(home, "Library/LaunchAgents", serviceName+".plist"))
			return err == nil
		}
	case "linux":
		home, err := os.UserHomeDir()
		if err == nil {
			_, err := os.Stat(filepath.Join(home, ".config/systemd/user", serviceName+".service"))
			return err == nil
		}
	}
	return false
}

func isDarwinServiceRunning(serviceName string) bool {
	// 1. Check system domain
	out, err := exec.Command("launchctl", "print", "system/"+serviceName).Output()
	if err == nil && (strings.Contains(string(out), "state = running") || strings.Contains(string(out), "pid = ")) {
		return true
	}

	// 2. Check current user domain
	uid := os.Getuid()
	out, err = exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", uid, serviceName)).Output()
	if err == nil && (strings.Contains(string(out), "state = running") || strings.Contains(string(out), "pid = ")) {
		return true
	}

	// 3. Check process table
	out, err = exec.Command("pgrep", "-f", "kizuna service run").Output()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		return true
	}

	return false
}

func isLinuxServiceRunning(serviceName string) bool {
	out, err := exec.Command("systemctl", "is-active", serviceName).Output()
	if err == nil && strings.TrimSpace(string(out)) == "active" {
		return true
	}
	out, err = exec.Command("systemctl", "--user", "is-active", serviceName).Output()
	if err == nil && strings.TrimSpace(string(out)) == "active" {
		return true
	}
	out, err = exec.Command("pgrep", "-f", "kizuna service run").Output()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		return true
	}
	return false
}
