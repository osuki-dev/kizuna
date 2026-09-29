package service

import (
	"fmt"
	"os"

	"github.com/kardianos/service"
)

// DaemonConfig holds service configuration
type DaemonConfig struct {
	Name        string
	DisplayName string
	Description string
	Arguments   []string
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
	svc service.Service
}

// NewManager creates a daemon service manager
func NewManager(cfg DaemonConfig, runFunc func()) (*Manager, error) {
	execPath, err := os.Executable()
	if err != nil {
		return nil, err
	}

	svcConfig := &service.Config{
		Name:        cfg.Name,
		DisplayName: cfg.DisplayName,
		Description: cfg.Description,
		Executable:  execPath,
		Arguments:   cfg.Arguments,
		Option: service.KeyValue{
			"Restart":    "always",
			"RestartSec": 5,
			"KeepAlive":  true,
		},
	}

	prg := &Program{runFunc: runFunc}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		return nil, err
	}

	return &Manager{svc: s}, nil
}

// Restart stops and starts the background service
func (m *Manager) Restart() error {
	_ = m.svc.Stop()
	return m.svc.Start()
}

// Install registers the service with the OS init system (systemd/launchd/windows service)
func (m *Manager) Install() error {
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
	if err != nil {
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
		return service.StatusUnknown, fmt.Errorf("unable to check status: %w", err)
	}
	return status, nil
}
