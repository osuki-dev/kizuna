package workload

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// Runner manages local workloads (Docker, Docker Compose, Bun, Node, native processes)
type Runner struct {
	mu           sync.RWMutex
	baseDir      string
	servicesFile string
	services     map[string]*entity.Service
	processes    map[string]*exec.Cmd
}

// NewWorkloadRunner initializes a new workload manager
func NewWorkloadRunner(baseDir string) domain.WorkloadRunner {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna", "workloads")
	}
	_ = os.MkdirAll(baseDir, 0755)
	_ = os.MkdirAll(filepath.Join(baseDir, "logs"), 0755)

	servicesFile := filepath.Join(baseDir, "services.json")
	r := &Runner{
		baseDir:      baseDir,
		servicesFile: servicesFile,
		services:     make(map[string]*entity.Service),
		processes:    make(map[string]*exec.Cmd),
	}
	r.loadServices()
	r.reconcile()
	return r
}

// Deploy executes the deployment pipeline for a given service
func (r *Runner) Deploy(ctx context.Context, svc *entity.Service, artifactReader io.Reader) error {
	r.mu.Lock()
	svc.State = entity.StateDeploying
	svc.UpdatedAt = time.Now()
	r.services[svc.Name] = svc
	r.saveServicesLocked()
	r.mu.Unlock()

	svcDir := filepath.Join(r.baseDir, svc.Name)
	_ = os.MkdirAll(svcDir, 0755)

	// 1. If an artifact stream is provided, unpack it into the service directory
	if artifactReader != nil {
		if err := unpackTarGz(artifactReader, svcDir); err != nil {
			r.updateState(svc.Name, entity.StateFailed)
			return fmt.Errorf("failed to unpack deployment artifact: %w", err)
		}
	}

	// 2. Deploy according to service type
	var err error
	switch svc.Type {
	case entity.TypeCompose:
		err = r.deployCompose(ctx, svc, svcDir)
	case entity.TypeDocker:
		err = r.deployDocker(ctx, svc, svcDir)
	case entity.TypeBun, entity.TypeNode, entity.TypeProcess:
		err = r.deployProcess(ctx, svc, svcDir)
	default:
		err = fmt.Errorf("unsupported service type: %s", svc.Type)
	}

	if err != nil {
		r.updateState(svc.Name, entity.StateFailed)
		return err
	}

	r.updateState(svc.Name, entity.StateRunning)
	return nil
}

func (r *Runner) deployCompose(ctx context.Context, svc *entity.Service, workDir string) error {
	composeFile := svc.ComposeFile
	if composeFile == "" {
		composeFile = "docker-compose.yml"
	}
	if !filepath.IsAbs(composeFile) {
		composeFile = filepath.Join(workDir, composeFile)
	}

	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composeFile, "up", "-d", "--remove-orphans")
	cmd.Dir = workDir
	cmd.Stdout = r.getLogWriter(svc.Name)
	cmd.Stderr = cmd.Stdout
	return cmd.Run()
}

func (r *Runner) deployDocker(ctx context.Context, svc *entity.Service, workDir string) error {
	imageName := svc.Image
	// If Dockerfile is provided or image not specified, build locally on target
	if svc.Dockerfile != "" || imageName == "" {
		df := svc.Dockerfile
		if df == "" {
			df = "Dockerfile"
		}
		imageName = fmt.Sprintf("kizuna/%s:latest", svc.Name)
		buildCmd := exec.CommandContext(ctx, "docker", "build", "--network=host", "-t", imageName, "-f", filepath.Join(workDir, df), workDir)
		buildCmd.Stdout = r.getLogWriter(svc.Name)
		buildCmd.Stderr = buildCmd.Stdout
		if err := buildCmd.Run(); err != nil {
			return fmt.Errorf("docker build failed: %w", err)
		}
	}

	maxSize := "50m"
	maxFile := "3"
	if svc.Logging != nil {
		if svc.Logging.MaxSize != "" {
			maxSize = svc.Logging.MaxSize
		}
		if svc.Logging.MaxFile > 0 {
			maxFile = strconv.Itoa(svc.Logging.MaxFile)
		}
	}

	replicas := svc.Replicas
	if replicas <= 0 {
		replicas = 1
	}

	// Clean up any stale replicas from previous scale down
	for i := replicas + 1; i <= 20; i++ {
		_ = exec.Command("docker", "rm", "-f", fmt.Sprintf("kizuna-%s-%d", svc.Name, i)).Run()
	}

	if replicas == 1 {
		containerName := fmt.Sprintf("kizuna-%s", svc.Name)
		_ = exec.Command("docker", "rm", "-f", containerName).Run()

		args := []string{
			"run", "-d",
			"--name", containerName,
			"--restart", "unless-stopped",
			"--log-opt", fmt.Sprintf("max-size=%s", maxSize),
			"--log-opt", fmt.Sprintf("max-file=%s", maxFile),
		}
		for _, port := range svc.Ports {
			args = append(args, "-p", port)
		}
		for k, v := range svc.Env {
			args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
		}
		args = append(args, imageName)

		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdout = r.getLogWriter(svc.Name)
		cmd.Stderr = cmd.Stdout
		return cmd.Run()
	}

	// Multi-replica local scaling with auto-port allocation & Caddy upstream pooling
	_ = exec.Command("docker", "rm", "-f", fmt.Sprintf("kizuna-%s", svc.Name)).Run()

	basePort := 3000
	containerPort := 3000
	if len(svc.Ports) > 0 {
		parts := strings.Split(svc.Ports[0], ":")
		if len(parts) >= 2 {
			if bp, err := strconv.Atoi(parts[0]); err == nil {
				basePort = bp
			}
			if cp, err := strconv.Atoi(parts[1]); err == nil {
				containerPort = cp
			}
		}
	}

	var upstreams []string
	for i := 1; i <= replicas; i++ {
		containerName := fmt.Sprintf("kizuna-%s-%d", svc.Name, i)
		_ = exec.Command("docker", "rm", "-f", containerName).Run()

		replicaPort := basePort + i - 1
		upstreams = append(upstreams, fmt.Sprintf("127.0.0.1:%d", replicaPort))

		args := []string{
			"run", "-d",
			"--name", containerName,
			"--restart", "unless-stopped",
			"--log-opt", fmt.Sprintf("max-size=%s", maxSize),
			"--log-opt", fmt.Sprintf("max-file=%s", maxFile),
			"-p", fmt.Sprintf("%d:%d", replicaPort, containerPort),
		}
		for k, v := range svc.Env {
			args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
		}
		args = append(args, imageName)

		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdout = r.getLogWriter(svc.Name)
		cmd.Stderr = cmd.Stdout
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to run replica %d: %w", i, err)
		}
	}

	if svc.Ingress != nil {
		svc.Ingress.Upstreams = upstreams
		if svc.Ingress.LBPolicy == "" {
			svc.Ingress.LBPolicy = "round_robin"
		}
	}

	return nil
}

func (r *Runner) deployProcess(ctx context.Context, svc *entity.Service, workDir string) error {
	r.mu.Lock()
	if existing, ok := r.processes[svc.Name]; ok && existing.Process != nil {
		_ = existing.Process.Kill()
	}
	r.mu.Unlock()

	cmdStr := ""
	if svc.Deploy != nil && svc.Deploy.Command != "" {
		cmdStr = svc.Deploy.Command
	} else if svc.Type == entity.TypeBun {
		cmdStr = "bun run start"
	} else if svc.Type == entity.TypeNode {
		cmdStr = "node index.js"
	} else {
		return fmt.Errorf("no start command configured for process service: %s", svc.Name)
	}

	parts := strings.Fields(cmdStr)
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	for k, v := range svc.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	if svc.Deploy != nil {
		for k, v := range svc.Deploy.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	logWriter := r.getLogWriter(svc.Name)
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start process: %w", err)
	}

	r.mu.Lock()
	r.processes[svc.Name] = cmd
	r.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		r.mu.Lock()
		if cur, ok := r.services[svc.Name]; ok && cur.State == entity.StateRunning {
			cur.State = entity.StateStopped
		}
		r.mu.Unlock()
	}()

	return nil
}

// Stop terminates a running service
func (r *Runner) Stop(ctx context.Context, serviceName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	svc, ok := r.services[serviceName]
	if !ok {
		return fmt.Errorf("service '%s' not found", serviceName)
	}

	switch svc.Type {
	case entity.TypeCompose:
		composeFile := svc.ComposeFile
		if composeFile == "" {
			composeFile = "docker-compose.yml"
		}
		_ = exec.CommandContext(ctx, "docker", "compose", "-f", filepath.Join(r.baseDir, svc.Name, composeFile), "down").Run()
	case entity.TypeDocker:
		containerName := fmt.Sprintf("kizuna-%s", svc.Name)
		_ = exec.CommandContext(ctx, "docker", "stop", containerName).Run()
	default:
		if p, ok := r.processes[serviceName]; ok && p.Process != nil {
			_ = p.Process.Kill()
		}
	}

	svc.State = entity.StateStopped
	svc.UpdatedAt = time.Now()
	r.saveServicesLocked()
	return nil
}

// GetStatus returns the current status of a service
func (r *Runner) GetStatus(ctx context.Context, serviceName string) (*entity.Service, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	svc, ok := r.services[serviceName]
	if !ok {
		return nil, fmt.Errorf("service '%s' not found", serviceName)
	}
	return svc, nil
}

// ListServices returns all managed services
func (r *Runner) ListServices(ctx context.Context) ([]*entity.Service, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var list []*entity.Service
	for _, s := range r.services {
		list = append(list, s)
	}
	return list, nil
}

// StreamLogs streams historical and live logs for a service
func (r *Runner) StreamLogs(ctx context.Context, serviceName string, writer io.Writer) error {
	logFile := filepath.Join(r.baseDir, "logs", fmt.Sprintf("%s.log", serviceName))
	f, err := os.Open(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintf(writer, "[kizuna] No logs yet for service '%s'\n", serviceName)
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		_, _ = fmt.Fprintln(writer, scanner.Text())
	}
	return scanner.Err()
}

func (r *Runner) updateState(name string, state entity.ServiceState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.services[name]; ok {
		s.State = state
		s.UpdatedAt = time.Now()
		r.saveServicesLocked()
	}
}

func (r *Runner) loadServices() {
	if r.servicesFile == "" {
		return
	}
	data, err := os.ReadFile(r.servicesFile)
	if err != nil {
		return
	}
	var stored map[string]*entity.Service
	if err := json.Unmarshal(data, &stored); err == nil {
		for k, v := range stored {
			r.services[k] = v
		}
	}
}

func (r *Runner) saveServicesLocked() {
	if r.servicesFile == "" {
		return
	}
	data, err := json.MarshalIndent(r.services, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(r.servicesFile, data, 0644)
}

func (r *Runner) reconcile() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "name=kizuna-", "--format", "{{.Names}}|{{.State}}|{{.Image}}|{{.Ports}}")
	out, err := cmd.Output()
	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	dockerStates := make(map[string]string)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		cName := parts[0]
		cState := parts[1]
		baseName := strings.TrimPrefix(cName, "kizuna-")
		if baseName == "caddy" {
			continue
		}
		if idx := strings.Index(baseName, "-replica-"); idx != -1 {
			baseName = baseName[:idx]
		}

		if cState == "running" {
			dockerStates[baseName] = "running"
		} else if dockerStates[baseName] != "running" {
			dockerStates[baseName] = cState
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for name, svc := range r.services {
		if svc.Type == entity.TypeDocker {
			if st, ok := dockerStates[name]; ok {
				if st == "running" {
					svc.State = entity.StateRunning
				} else {
					svc.State = entity.StateStopped
				}
			} else {
				svc.State = entity.StateStopped
			}
		}
	}

	for baseName, st := range dockerStates {
		if _, exists := r.services[baseName]; !exists {
			svcState := entity.StateStopped
			if st == "running" {
				svcState = entity.StateRunning
			}
			r.services[baseName] = &entity.Service{
				Name:      baseName,
				Type:      entity.TypeDocker,
				State:     svcState,
				UpdatedAt: time.Now(),
			}
		}
	}

	r.saveServicesLocked()
}

func (r *Runner) getLogWriter(serviceName string) io.Writer {
	logFile := filepath.Join(r.baseDir, "logs", fmt.Sprintf("%s.log", serviceName))
	f, _ := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	return f
}

func unpackTarGz(r io.Reader, destDir string) error {
	cmd := exec.Command("tar", "-xzf", "-", "-C", destDir)
	cmd.Stdin = r
	return cmd.Run()
}
