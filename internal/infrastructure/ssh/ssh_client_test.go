package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	gossh "golang.org/x/crypto/ssh"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellArgumentsAreLiteral(t *testing.T) {
	got := command("docker", "run", "-e", "TOKEN=$(touch /tmp/pwn);`id`'value")
	if got != "'docker' 'run' '-e' 'TOKEN=$(touch /tmp/pwn);`id`'\"'\"'value'" {
		t.Fatalf("unsafe quoting: %s", got)
	}
}
func TestRejectInvalidNameAndUnsupportedWorkloadBeforeSSH(t *testing.T) {
	c := NewClient(&entity.TargetHost{Host: "test"})
	c.runCommand = func(context.Context, string) (string, error) { t.Fatal("unexpected remote command"); return "", nil }
	for _, svc := range []*entity.Service{{Name: "app;id", Type: entity.TypeDocker}, {Name: "app", Type: entity.TypeDocker, Replicas: 2}, {Name: "app", Type: entity.TypeNode}} {
		if _, err := c.Deploy(context.Background(), svc, nil); err == nil {
			t.Fatalf("accepted invalid service %+v", svc)
		}
	}
}
func TestDockerDeploymentFailureRestoresOldContainer(t *testing.T) {
	var commands []string
	c := NewClient(&entity.TargetHost{Host: "test"})
	c.runCommand = func(ctx context.Context, cmd string) (string, error) {
		commands = append(commands, cmd)
		switch {
		case strings.Contains(cmd, "'image' 'inspect'"):
			return "sha256:fixed", nil
		case strings.Contains(cmd, "'ps' '-a'"):
			return "old-id", nil
		case strings.Contains(cmd, "{{.State.Running}}"):
			return "true", nil
		case strings.Contains(cmd, "'run' '-d'"):
			return "", errors.New("new container failed")
		default:
			return "", nil
		}
	}
	_, err := c.Deploy(context.Background(), &entity.Service{Name: "app", Type: entity.TypeDocker, Image: "app:latest"}, nil)
	if err == nil {
		t.Fatal("deployment reported success")
	}
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "'start' 'kizuna-app'") {
		t.Fatal("did not restart old container", joined)
	}
	if strings.Contains(joined, "'rm' 'kizuna-app'") {
		t.Fatal("deleted old container before replacement", joined)
	}
	if !strings.Contains(joined, "'sha256:fixed'") {
		t.Fatal("did not deploy immutable image", joined)
	}
}
func TestDockerSuccessfulDeploymentPinsImageAndRedactsMetadata(t *testing.T) {
	c := NewClient(&entity.TargetHost{Host: "test"})
	c.runCommand = func(ctx context.Context, cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "'image' 'inspect'"):
			return "sha256:fixed", nil
		case strings.Contains(cmd, ".State.Status"):
			return `{"status":"running","health":"healthy","restarting":false,"restart_count":0}`, nil
		case strings.Contains(cmd, "'run' '-d'"):
			return "new-id", nil
		default:
			return "", nil
		}
	}
	var metadata string
	c.streamCommand = func(ctx context.Context, cmd string, r io.Reader) (string, error) {
		b, _ := io.ReadAll(r)
		metadata = string(b)
		return "", nil
	}
	svc := &entity.Service{Name: "app", Type: entity.TypeDocker, Image: "app:latest", Env: map[string]string{"SECRET": "do-not-persist"}}
	result, err := c.Deploy(context.Background(), svc, nil)
	if err != nil || !result.Success {
		t.Fatalf("deploy: %v %+v", err, result)
	}
	if svc.Image != "sha256:fixed" {
		t.Fatalf("image not pinned: %s", svc.Image)
	}
	if strings.Contains(metadata, "do-not-persist") {
		t.Fatal("metadata leaked environment secret")
	}
}
func TestComposeUsesExplicitDirectoryAndSelectedServices(t *testing.T) {
	var commands []string
	c := NewClient(&entity.TargetHost{Host: "test"})
	c.runCommand = func(ctx context.Context, cmd string) (string, error) {
		commands = append(commands, cmd)
		return "", nil
	}
	c.streamCommand = func(context.Context, string, io.Reader) (string, error) { return "", nil }
	svc := &entity.Service{Name: "app", Type: entity.TypeCompose, Root: "/local/path", ComposeFile: "compose.yaml", Deploy: &entity.DeployConfig{Dest: "/srv/app"}, Compose: &entity.ComposeConfig{ProjectName: "existing", Services: []string{"app", "worker"}, EnvFiles: []string{"/srv/secrets/app.env"}}}
	if _, err := c.Deploy(context.Background(), svc, nil); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(commands, "\n")
	for _, expected := range []string{"cd '/srv/app'", "'--project-name' 'existing'", "'--env-file' '/srv/secrets/app.env'", "'--wait'", "'app' 'worker'"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing %s: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "--remove-orphans") {
		t.Fatal("destructive remove-orphans enabled")
	}
	if _, err := c.Deploy(context.Background(), &entity.Service{Name: "app", Type: entity.TypeCompose, Root: "/local/path"}, nil); err == nil {
		t.Fatal("treated local root as remote deployment directory")
	}
}
func TestComposeStopAndStatusUseRemoteMetadata(t *testing.T) {
	c := NewClient(&entity.TargetHost{Host: "test"})
	c.runCommand = func(ctx context.Context, cmd string) (string, error) {
		if strings.HasPrefix(cmd, "cat ") {
			return `{"name":"app","type":"compose","root":"/srv/app","compose":{"project_name":"existing","services":["worker"]}}`, nil
		}
		if strings.Contains(cmd, "'ps'") {
			return "{\"State\":\"running\",\"Health\":\"healthy\"}\n{\"State\":\"running\",\"Health\":\"unhealthy\"}", nil
		}
		if !strings.Contains(cmd, "'stop' 'worker'") {
			t.Fatalf("wrong stop command %s", cmd)
		}
		return "", nil
	}
	if err := c.Stop(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	svc, err := c.GetStatus(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if svc.State != entity.StateFailed {
		t.Fatalf("unhealthy Compose reported as %s", svc.State)
	}
}
func TestStatusDoesNotHideSSHFailure(t *testing.T) {
	c := NewClient(&entity.TargetHost{Host: "test"})
	c.runCommand = func(context.Context, string) (string, error) { return "", errors.New("SSH failed") }
	if _, err := c.GetStatus(context.Background(), "app"); err == nil {
		t.Fatal("SSH failure reported as stopped")
	}
}

func TestHostKeyVerificationRejectsUnknownAndChangedHosts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), append([]byte("trusted.example "), gossh.MarshalAuthorizedKey(trusted)...), 0600); err != nil {
		t.Fatal(err)
	}
	verify, err := hostKeyCallback()
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 22}
	if err := verify("trusted.example:22", addr, trusted); err != nil {
		t.Fatalf("trusted host rejected: %v", err)
	}
	if err := verify("unknown.example:22", addr, trusted); err == nil {
		t.Fatal("unknown host accepted")
	}
	pub, _, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify("trusted.example:22", addr, changed); err == nil {
		t.Fatal("changed host key accepted")
	}
}

func TestConnectCancelsBlockedHandshake(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	client := NewClient(&entity.TargetHost{Host: "127.0.0.1", Port: addr.Port})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.connect(ctx); done <- err }()
	select {
	case conn := <-accepted:
		defer func() { _ = conn.Close() }()
	case <-time.After(2 * time.Second):
		t.Fatal("did not establish TCP connection")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSH banner wait ignored context cancellation")
	}
}

func TestWaitContainerRejectsTransientRunningAndRestart(t *testing.T) {
	for _, second := range []string{
		`{"status":"restarting","restarting":true,"restart_count":0}`,
		`{"status":"running","restarting":false,"restart_count":1}`,
	} {
		t.Run(second, func(t *testing.T) {
			client := NewClient(&entity.TargetHost{Host: "test"})
			calls := 0
			client.runCommand = func(context.Context, string) (string, error) {
				calls++
				if calls == 1 {
					return `{"status":"running","restarting":false,"restart_count":0}`, nil
				}
				return second, nil
			}
			if err := client.waitContainer(context.Background(), "app"); err == nil {
				t.Fatal("transient running state accepted")
			}
			if calls < 2 {
				t.Fatal("readiness did not verify stability")
			}
		})
	}
}
func TestWaitContainerWithoutHealthRequiresStableWindow(t *testing.T) {
	client := NewClient(&entity.TargetHost{Host: "test"})
	calls := 0
	client.runCommand = func(context.Context, string) (string, error) {
		calls++
		return `{"status":"running","restarting":false,"restart_count":0}`, nil
	}
	started := time.Now()
	if err := client.waitContainer(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	if calls < 2 || time.Since(started) < time.Second {
		t.Fatal("accepted container without a stability window")
	}
}
