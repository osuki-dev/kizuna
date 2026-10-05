package workload

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

type mockContainer struct {
	Running      bool
	Label        string
	Health       string
	InspectCount int
	RestartCount int
}

// A helper subprocess gives the production runner a Docker CLI with no daemon.
func TestDockerHelper(t *testing.T) {
	if os.Getenv("KIZUNA_DOCKER_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	data, _ := os.ReadFile(os.Getenv("KIZUNA_DOCKER_STATE"))
	containers := map[string]mockContainer{}
	_ = json.Unmarshal(data, &containers)
	if containers == nil {
		containers = map[string]mockContainer{}
	}
	log, _ := os.OpenFile(os.Getenv("KIZUNA_DOCKER_CALLS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	_, _ = fmt.Fprintln(log, strings.Join(args, " "))
	_ = log.Close()
	save := func() {
		data, _ := json.Marshal(containers)
		_ = os.WriteFile(os.Getenv("KIZUNA_DOCKER_STATE"), data, 0600)
	}
	switch args[0] {
	case "ps":
		for name, c := range containers {
			if strings.Contains(args[len(args)-1], ".Label") {
				fmt.Printf("%s|%s\n", name, c.Label)
			} else {
				state := "exited"
				if c.Running {
					state = "running"
				}
				fmt.Printf("%s|%s|image|\n", name, state)
			}
		}
	case "pull", "build":
	case "image":
		fmt.Println("sha256:immutable")
	case "inspect":
		c, ok := containers[args[len(args)-1]]
		if !ok {
			os.Exit(1)
		}
		status := "exited"
		if c.Running {
			status = "running"
		}
		c.InspectCount++
		if os.Getenv("KIZUNA_DOCKER_CRASH_AFTER_FIRST_INSPECT") == "1" && c.InspectCount > 1 {
			c.RestartCount = 1
		}
		containers[args[len(args)-1]] = c
		save()
		result := map[string]any{"Running": c.Running, "Status": status}
		if c.Health != "" {
			result["Health"] = map[string]string{"Status": c.Health}
		}
		data, _ := json.Marshal(map[string]any{"State": result, "RestartCount": c.RestartCount})
		fmt.Println(string(data))
	case "stop", "start":
		for _, name := range args[1:] {
			c, ok := containers[name]
			if !ok {
				os.Exit(1)
			}
			c.Running = args[0] == "start"
			containers[name] = c
		}
		save()
	case "rename":
		c, ok := containers[args[1]]
		if !ok {
			os.Exit(1)
		}
		delete(containers, args[1])
		containers[args[2]] = c
		save()
	case "rm":
		for _, name := range args[1:] {
			if name != "-f" {
				delete(containers, name)
			}
		}
		save()
	case "run":
		name, label := "", ""
		for i, arg := range args {
			if arg == "--name" {
				name = args[i+1]
			}
			if arg == "--label" {
				label = strings.TrimPrefix(args[i+1], "kizuna.service=")
			}
		}
		if os.Getenv("KIZUNA_DOCKER_FAIL_RUN") == "1" {
			os.Exit(1)
		}
		containers[name] = mockContainer{Running: true, Label: label, Health: os.Getenv("KIZUNA_DOCKER_HEALTH")}
		save()
	case "logs":
		fmt.Println("runtime output")
	case "compose":
		if strings.Contains(strings.Join(args, " "), "logs") {
			fmt.Println("compose runtime output")
		}
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func mockDocker(t *testing.T, containers map[string]mockContainer) (string, string) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	calls := filepath.Join(dir, "calls")
	data, _ := json.Marshal(containers)
	if err := os.WriteFile(state, data, 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run=^TestDockerHelper$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KIZUNA_DOCKER_HELPER", "1")
	t.Setenv("KIZUNA_DOCKER_STATE", state)
	t.Setenv("KIZUNA_DOCKER_CALLS", calls)
	return state, calls
}

func TestDockerFailedUpdateRestoresOldContainer(t *testing.T) {
	state, _ := mockDocker(t, map[string]mockContainer{"kizuna-api": {Running: true, Label: "api"}})
	runner := NewWorkloadRunner(t.TempDir())
	t.Setenv("KIZUNA_DOCKER_FAIL_RUN", "1")
	err := runner.Deploy(context.Background(), &entity.Service{Name: "api", Type: entity.TypeDocker, Image: "example:v2"}, nil)
	if err == nil {
		t.Fatal("expected failed update")
	}
	data, _ := os.ReadFile(state)
	containers := map[string]mockContainer{}
	_ = json.Unmarshal(data, &containers)
	if len(containers) != 1 || !containers["kizuna-api"].Running {
		t.Fatalf("old container not restored: %s", data)
	}
}

func TestDockerScaleDownAndStopAllReplicas(t *testing.T) {
	state, calls := mockDocker(t, map[string]mockContainer{"kizuna-api-1": {Running: true}, "kizuna-api-35": {Running: true}})
	runner := NewWorkloadRunner(t.TempDir())
	svc := &entity.Service{Name: "api", Type: entity.TypeDocker, Image: "example:v2"}
	if err := runner.Deploy(context.Background(), svc, nil); err != nil {
		t.Fatal(err)
	}
	if svc.Image != "sha256:immutable" {
		t.Fatalf("image not pinned: %s", svc.Image)
	}
	data, _ := os.ReadFile(state)
	containers := map[string]mockContainer{}
	_ = json.Unmarshal(data, &containers)
	if len(containers) != 1 || !containers["kizuna-api"].Running {
		t.Fatalf("scale down retained old replicas: %s", data)
	}
	if err := runner.Stop(context.Background(), "api"); err != nil {
		t.Fatal(err)
	}
	status, err := runner.GetStatus(context.Background(), "api")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != entity.StateStopped {
		t.Fatalf("state %s", status.State)
	}
	var output bytes.Buffer
	if err := runner.StreamLogs(context.Background(), "api", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "runtime output") {
		t.Fatal(output.String())
	}
	commands, _ := os.ReadFile(calls)
	if !strings.Contains(string(commands), "stop kizuna-api-35") {
		t.Fatal(string(commands))
	}
}

func TestDockerUnhealthyUpdateRestoresOldContainer(t *testing.T) {
	state, _ := mockDocker(t, map[string]mockContainer{"kizuna-api": {Running: true, Label: "api"}})
	runner := NewWorkloadRunner(t.TempDir())
	t.Setenv("KIZUNA_DOCKER_HEALTH", "unhealthy")
	if err := runner.Deploy(context.Background(), &entity.Service{Name: "api", Type: entity.TypeDocker, Image: "example:v2"}, nil); err == nil {
		t.Fatal("unhealthy deploy succeeded")
	}
	data, _ := os.ReadFile(state)
	containers := map[string]mockContainer{}
	_ = json.Unmarshal(data, &containers)
	if !containers["kizuna-api"].Running || len(containers) != 1 {
		t.Fatal(string(data))
	}
}

func TestSensitivePersistenceAndWriteError(t *testing.T) {
	dir := t.TempDir()
	r := NewWorkloadRunner(dir).(*Runner)
	r.services["api"] = &entity.Service{Name: "api", Type: entity.TypeProcess, Env: map[string]string{"PASSWORD": "secret"}}
	if err := r.saveServicesLocked(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(r.servicesFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions %o", info.Mode().Perm())
	}
	r.servicesFile = filepath.Join(dir, "missing", "services.json")
	if err := r.saveServicesLocked(); err == nil {
		t.Fatal("write error ignored")
	}
}

func TestArchiveRejectsExistingSymlink(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	_ = os.Mkdir(dest, 0700)
	_ = os.Symlink(root, filepath.Join(dest, "link"))
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "link/escape", Size: 1, Mode: 0600})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	if err := unpackTarGz(&buf, dest); err == nil {
		t.Fatal("symlink traversal accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Fatal("escaped archive root")
	}
}

func TestComposeUsesSelectedServicesAndPreservesProject(t *testing.T) {
	_, calls := mockDocker(t, nil)
	r := NewWorkloadRunner(t.TempDir())
	svc := &entity.Service{Name: "app", Type: entity.TypeCompose, Compose: &entity.ComposeConfig{ProjectName: "existing", Services: []string{"app", "worker"}, EnvFiles: []string{"production.env"}, Profiles: []string{"web"}}}
	if err := r.Deploy(context.Background(), svc, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	commands, _ := os.ReadFile(calls)
	if strings.Contains(string(commands), "remove-orphans") || strings.Contains(string(commands), " down") {
		t.Fatal(string(commands))
	}
	for _, expect := range []string{"--project-name existing", "--env-file", "--profile web", "--wait-timeout 60 app worker", "stop app worker"} {
		if !strings.Contains(string(commands), expect) {
			t.Fatalf("missing %q: %s", expect, commands)
		}
	}
}

func TestNativeRedeployAndStopHaveTruthfulStatus(t *testing.T) {
	mockDocker(t, nil)
	runner := NewWorkloadRunner(t.TempDir())
	for i := 0; i < 3; i++ {
		svc := &entity.Service{Name: "worker", Type: entity.TypeProcess, Deploy: &entity.DeployConfig{Command: "sleep 10"}}
		if err := runner.Deploy(context.Background(), svc, nil); err != nil {
			t.Fatal(err)
		}
		status, err := runner.GetStatus(context.Background(), "worker")
		if err != nil {
			t.Fatal(err)
		}
		if status.State != entity.StateRunning {
			t.Fatalf("replacement state %s", status.State)
		}
	}
	if err := runner.Stop(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	status, err := runner.GetStatus(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != entity.StateStopped {
		t.Fatalf("stopped process state %s", status.State)
	}
}

func TestInvalidNameRejectedBeforeFilesystemWrites(t *testing.T) {
	runner := NewWorkloadRunner(t.TempDir())
	if err := runner.Deploy(context.Background(), &entity.Service{Name: "../escape", Type: entity.TypeProcess}, nil); err == nil {
		t.Fatal("path traversal service name accepted")
	}
}

func TestDockerPreservesExplicitUpstreams(t *testing.T) {
	for _, replicas := range []int{1, 2} {
		t.Run(fmt.Sprintf("replicas-%d", replicas), func(t *testing.T) {
			mockDocker(t, nil)
			runner := NewWorkloadRunner(t.TempDir())
			svc := &entity.Service{Name: "api", Type: entity.TypeDocker, Image: "example:v2", Replicas: replicas, Ports: []string{"8080:80"}, Ingress: &entity.IngressConfig{Upstreams: []string{"10.0.0.20:8080"}}}
			if err := runner.Deploy(context.Background(), svc, nil); err != nil {
				t.Fatal(err)
			}
			if len(svc.Ingress.Upstreams) != 1 || svc.Ingress.Upstreams[0] != "10.0.0.20:8080" {
				t.Fatalf("explicit upstream overwritten: %v", svc.Ingress.Upstreams)
			}
		})
	}
}

func TestDockerStateWriteFailureRestoresOldContainer(t *testing.T) {
	state, calls := mockDocker(t, map[string]mockContainer{"kizuna-api": {Running: true, Label: "api"}})
	r := NewWorkloadRunner(t.TempDir()).(*Runner)
	svc := &entity.Service{Name: "api", Type: entity.TypeDocker, Image: "example:v2"}
	r.services["api"] = svc
	r.servicesFile = filepath.Join(r.baseDir, "missing", "services.json")
	if err := r.deployDocker(context.Background(), svc, r.baseDir); err == nil {
		t.Fatal("state write failure ignored")
	}
	data, _ := os.ReadFile(state)
	containers := map[string]mockContainer{}
	_ = json.Unmarshal(data, &containers)
	if len(containers) != 1 || !containers["kizuna-api"].Running {
		t.Fatalf("old deployment not restored: %s", data)
	}
	commands, _ := os.ReadFile(calls)
	if strings.Contains(string(commands), "rm kizuna-api-previous-") {
		t.Fatalf("old deployment deleted before state publication: %s", commands)
	}
}

func TestNativeCommandQuotingAndParseErrors(t *testing.T) {
	mockDocker(t, nil)
	runner := NewWorkloadRunner(t.TempDir())
	bad := &entity.Service{Name: "bad", Type: entity.TypeProcess, Deploy: &entity.DeployConfig{Command: "echo 'unterminated"}}
	if err := runner.Deploy(context.Background(), bad, nil); err == nil || !strings.Contains(err.Error(), "parse process command") {
		t.Fatalf("invalid quoting accepted: %v", err)
	}
	svc := &entity.Service{Name: "quoted", Type: entity.TypeProcess, Deploy: &entity.DeployConfig{Command: "echo 'hello world'"}}
	if err := runner.Deploy(context.Background(), svc, nil); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runner.StreamLogs(context.Background(), svc.Name, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "hello world\n" {
		t.Fatalf("quoted argument was split: %q", output.String())
	}
}

func TestDockerCrashLoopAfterFirstInspectRestoresOldContainer(t *testing.T) {
	state, _ := mockDocker(t, map[string]mockContainer{"kizuna-api": {Running: true, Label: "api"}})
	runner := NewWorkloadRunner(t.TempDir())
	t.Setenv("KIZUNA_DOCKER_CRASH_AFTER_FIRST_INSPECT", "1")
	err := runner.Deploy(context.Background(), &entity.Service{Name: "api", Type: entity.TypeDocker, Image: "example:v2"}, nil)
	if err == nil || !strings.Contains(err.Error(), "restarted during startup") {
		t.Fatalf("crash loop passed readiness: %v", err)
	}
	data, _ := os.ReadFile(state)
	containers := map[string]mockContainer{}
	_ = json.Unmarshal(data, &containers)
	if len(containers) != 1 || !containers["kizuna-api"].Running {
		t.Fatalf("old deployment not restored: %s", data)
	}
}
