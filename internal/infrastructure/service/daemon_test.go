package service

import (
	"testing"
	"time"
)

func TestNewManagerLifecycle(t *testing.T) {
	ran := make(chan bool, 1)
	cfg := DaemonConfig{
		Name:        "test-service",
		DisplayName: "Test Service",
		Description: "A unit test service",
		Arguments:   []string{"server", "run"},
	}

	mgr, err := NewManager(cfg, func() {
		ran <- true
	})
	if err != nil {
		t.Fatalf("failed to create service manager: %v", err)
	}

	if mgr == nil {
		t.Fatalf("expected non-nil service manager")
	}

	// Test Program execution directly
	prg := &Program{
		runFunc: func() {
			ran <- true
		},
	}

	if err := prg.Start(nil); err != nil {
		t.Fatalf("prg.Start failed: %v", err)
	}

	select {
	case <-ran:
		// success
	case <-time.After(1 * time.Second):
		t.Errorf("expected runFunc to be invoked by Program.Start")
	}

	if err := prg.Stop(nil); err != nil {
		t.Fatalf("prg.Stop failed: %v", err)
	}
}

func TestUserServiceConfig(t *testing.T) {
	cfg := DaemonConfig{
		Name:        "test-user-svc",
		DisplayName: "Test User Service",
		Description: "A unit test service",
		Arguments:   []string{"service", "run"},
		UserService: true,
	}

	mgr, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatalf("failed to create user service manager: %v", err)
	}
	if mgr == nil {
		t.Fatalf("expected non-nil service manager")
	}

	// Verify detection logic doesn't panic
	_ = DetectUserService("test-user-svc")
	_ = IsSystemInstalled("test-user-svc")
	_ = IsInstalled("test-user-svc")
}

