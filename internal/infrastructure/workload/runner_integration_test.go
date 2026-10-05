//go:build integration

package workload

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// This opt-in test uses a unique application container and an existing Bun
// image. It does not create databases, touch other containers, or pull images.
func TestDockerReplacementRecoveryIntegration(t *testing.T) {
	if os.Getenv("KIZUNA_DOCKER_E2E") != "1" {
		t.Skip("set KIZUNA_DOCKER_E2E=1 to run the isolated Docker integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", "oven/bun:latest").Run(); err != nil {
		t.Fatal("integration test requires an existing oven/bun:latest image")
	}
	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portListener.Addr().(*net.TCPAddr).Port
	if err := portListener.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	runner := NewWorkloadRunner(t.TempDir()).(*Runner)
	workDir := filepath.Join(runner.baseDir, name)
	if err := os.MkdirAll(workDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", "kizuna-"+name).Run()
		_ = exec.CommandContext(cleanupCtx, "docker", "image", "rm", "kizuna/"+name+":latest").Run()
	})
	dockerfile := `FROM oven/bun:latest
HEALTHCHECK --interval=1s --timeout=1s --retries=2 CMD bun -e 'const r = await fetch("http://127.0.0.1:8080/"); process.exit(r.ok ? 0 : 1)'
CMD ["bun", "-e", "Bun.serve({port:8080,fetch:()=>new Response('original-version')})"]
`

	if err := os.WriteFile(filepath.Join(workDir, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		t.Fatal(err)
	}
	svc := &entity.Service{Name: name, Type: entity.TypeDocker, Dockerfile: "Dockerfile", Ports: []string{fmt.Sprintf("127.0.0.1:%d:8080", port)}}
	if err := runner.Deploy(ctx, svc, nil); err != nil {
		log, _ := os.ReadFile(filepath.Join(runner.baseDir, "logs", name+".log"))
		t.Fatalf("initial deployment failed: %v\n%s", err, log)
	}
	assertOriginal := func() {
		t.Helper()
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK || string(body) != "original-version" {
			t.Fatalf("old application unavailable: %d %q %v", resp.StatusCode, body, err)
		}
	}
	assertOriginal()
	originalImage := svc.Image
	if originalImage == "" {
		t.Fatal("image was not pinned")
	}
	if err := os.WriteFile(filepath.Join(workDir, "Dockerfile"), []byte("FROM oven/bun:latest\nCMD [\"sh\", \"-c\", \"exit 23\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	replacement := &entity.Service{Name: name, Type: entity.TypeDocker, Dockerfile: "Dockerfile", Ports: svc.Ports}
	if err := runner.Deploy(ctx, replacement, nil); err == nil {
		t.Fatal("failed replacement reported success")
	}
	// Restarting the original container restarts Docker's health probe as well.
	if err := waitContainerReady(ctx, "kizuna-"+name); err != nil {
		t.Fatal(err)
	}
	assertOriginal()
	state, err := runner.GetStatus(ctx, name)
	if err != nil || state.State != entity.StateRunning || state.Image != originalImage {
		t.Fatalf("old state not restored: %+v %v", state, err)
	}
}
