package workload

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

func TestWorkloadRunner_PersistenceAndReload(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "kizuna-workload-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	runner := NewWorkloadRunner(tempDir)

	// Deploy a dummy process service
	svc := &entity.Service{
		Name:  "test-api",
		Type:  entity.TypeProcess,
		Ports: []string{"8080:8080"},
		Deploy: &entity.DeployConfig{
			Command: "echo 'hello world'",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := runner.Deploy(ctx, svc, nil); err != nil {
		t.Fatalf("deploy failed: %v", err)
	}

	st, err := runner.GetStatus(ctx, "test-api")
	if err != nil {
		t.Fatalf("get status failed: %v", err)
	}
	if st.Name != "test-api" {
		t.Fatalf("expected 'test-api', got %s", st.Name)
	}

	// Verify services.json exists
	sf := filepath.Join(tempDir, "services.json")
	if _, err := os.Stat(sf); os.IsNotExist(err) {
		t.Fatalf("services.json was not created")
	}

	// Create a NEW runner pointing to the same directory to verify state recovery
	reloadedRunner := NewWorkloadRunner(tempDir)
	reloadedSt, err := reloadedRunner.GetStatus(ctx, "test-api")
	if err != nil {
		t.Fatalf("reloaded get status failed: %v", err)
	}
	if reloadedSt.Name != "test-api" {
		t.Fatalf("expected restored service 'test-api', got %s", reloadedSt.Name)
	}
}
