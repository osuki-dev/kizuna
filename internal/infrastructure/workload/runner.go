package workload

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anmitsu/go-shlex"
	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// Runner manages local workloads (Docker, Docker Compose, Bun, Node, native processes)
type Runner struct {
	mu           sync.RWMutex
	operationMu  sync.Mutex
	initErr      error
	baseDir      string
	servicesFile string
	services     map[string]*entity.Service
	processes    map[string]*exec.Cmd
	processDone  map[string]chan struct{}
}

// NewWorkloadRunner initializes a new workload manager
func NewWorkloadRunner(baseDir string) domain.WorkloadRunner {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna", "workloads")
	}
	mkdirErr := os.MkdirAll(baseDir, 0700)
	if mkdirErr == nil {
		mkdirErr = os.MkdirAll(filepath.Join(baseDir, "logs"), 0700)
	}

	servicesFile := filepath.Join(baseDir, "services.json")
	r := &Runner{
		baseDir:      baseDir,
		servicesFile: servicesFile,
		services:     make(map[string]*entity.Service),
		processes:    make(map[string]*exec.Cmd),
		processDone:  make(map[string]chan struct{}),
	}
	r.initErr = mkdirErr
	if r.initErr == nil {
		r.initErr = r.loadServices()
	}
	if r.initErr == nil {
		r.reconcile()
	}
	return r
}

// Deploy executes the deployment pipeline for a given service
func (r *Runner) Deploy(ctx context.Context, svc *entity.Service, artifactReader io.Reader) error {
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if svc == nil || !validServiceName(svc.Name) {
		return fmt.Errorf("invalid service name")
	}
	if r.initErr != nil {
		return r.initErr
	}
	r.mu.Lock()
	previous := r.services[svc.Name]
	if previous != nil {
		copied := *previous
		previous = &copied
	}
	svc.State = entity.StateDeploying
	svc.UpdatedAt = time.Now()
	r.services[svc.Name] = svc
	saveErr := r.saveServicesLocked()
	r.mu.Unlock()
	if saveErr != nil {
		r.mu.Lock()
		if previous == nil {
			delete(r.services, svc.Name)
		} else {
			r.services[svc.Name] = previous
		}
		r.mu.Unlock()
		return saveErr
	}

	svcDir := filepath.Join(r.baseDir, svc.Name)
	if err := os.MkdirAll(svcDir, 0700); err != nil {
		return err
	}

	// 1. If an artifact stream is provided, unpack it into the service directory
	if artifactReader != nil {
		if err := unpackTarGz(artifactReader, svcDir); err != nil {
			return errors.Join(fmt.Errorf("failed to unpack deployment artifact: %w", err), r.updateState(svc.Name, entity.StateFailed))
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
		// A cleanup failure after durable publication leaves the new version running.
		if svc.Type == entity.TypeDocker && svc.State == entity.StateRunning {
			return err
		}
		if previous != nil && svc.Type == entity.TypeDocker {
			r.mu.Lock()
			r.services[svc.Name] = previous
			saveErr := r.saveServicesLocked()
			r.mu.Unlock()
			return errors.Join(err, saveErr)
		}
		return errors.Join(err, r.updateState(svc.Name, entity.StateFailed))
	}

	// Docker publishes its state before deleting rollback containers.
	if svc.Type == entity.TypeDocker {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A process may already have exited while deployment was finishing.
	if svc.Type == entity.TypeProcess || svc.Type == entity.TypeBun || svc.Type == entity.TypeNode {
		if r.processes[svc.Name] == nil {
			svc.State = entity.StateStopped
		} else {
			svc.State = entity.StateRunning
		}
	} else {
		svc.State = entity.StateRunning
	}
	svc.UpdatedAt = time.Now()
	return r.saveServicesLocked()
}

func (r *Runner) composeArgs(svc *entity.Service, workDir string) []string {
	file := svc.ComposeFile
	if file == "" {
		file = "docker-compose.yml"
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(workDir, file)
	}
	args := []string{"compose", "-f", file}
	if svc.Compose != nil {
		if svc.Compose.ProjectName != "" {
			args = append(args, "--project-name", svc.Compose.ProjectName)
		}
		for _, file := range svc.Compose.EnvFiles {
			if !filepath.IsAbs(file) {
				file = filepath.Join(workDir, file)
			}
			args = append(args, "--env-file", file)
		}
		for _, profile := range svc.Compose.Profiles {
			args = append(args, "--profile", profile)
		}
	}
	return args
}

func selectedServices(svc *entity.Service) []string {
	if svc.Compose != nil {
		return svc.Compose.Services
	}
	return nil
}

func (r *Runner) dockerRun(ctx context.Context, svc *entity.Service, workDir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = workDir
	writer := r.getLogWriter(svc.Name)
	if closer, ok := writer.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s failed: %w", args[0], err)
	}
	return nil
}

func (r *Runner) deployCompose(ctx context.Context, svc *entity.Service, workDir string) error {
	args := append(r.composeArgs(svc, workDir), "up", "-d", "--wait", "--wait-timeout", "60")
	args = append(args, selectedServices(svc)...)
	return r.dockerRun(ctx, svc, workDir, args...)
}

// managedContainers also recognizes the old numbered replica names. Labels avoid
// confusing a service named app-2 with replica 2 of app for new deployments.
func (r *Runner) managedContainers(ctx context.Context, name string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--format", "{{.Names}}|{{.Label \"kizuna.service\"}}").Output()
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var names []string
	base := "kizuna-" + name
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 2)
		container := parts[0]
		if strings.Contains(container, "-previous-") {
			continue
		}
		if len(parts) == 2 && parts[1] != "" {
			if parts[1] == name {
				names = append(names, container)
			}
			continue
		}
		if container == base {
			names = append(names, container)
			continue
		}
		suffix := strings.TrimPrefix(container, base+"-")
		suffix = strings.TrimPrefix(suffix, "replica-")
		if container != suffix {
			if n, err := strconv.Atoi(suffix); err == nil && n > 0 {
				names = append(names, container)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

func (r *Runner) deployDocker(ctx context.Context, svc *entity.Service, workDir string) error {
	image := svc.Image
	if svc.Dockerfile != "" || image == "" {
		df := svc.Dockerfile
		if df == "" {
			df = "Dockerfile"
		}
		if !filepath.IsAbs(df) {
			df = filepath.Join(workDir, df)
		}
		image = fmt.Sprintf("kizuna/%s:latest", svc.Name)
		if err := r.dockerRun(ctx, svc, workDir, "build", "--network=host", "-t", image, "-f", df, workDir); err != nil {
			return err
		}
	} else if !strings.HasPrefix(image, "sha256:") {
		if err := r.dockerRun(ctx, svc, workDir, "pull", image); err != nil {
			return err
		}
	}
	id, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image).Output()
	if err != nil || strings.TrimSpace(string(id)) == "" {
		return fmt.Errorf("resolve image %q: %w", image, err)
	}
	image = strings.TrimSpace(string(id))
	old, err := r.managedContainers(ctx, svc.Name)
	if err != nil {
		return err
	}
	// Preserve all old replicas until the complete replacement is ready. Fixed
	// published ports require stopping them during this update.
	backups := map[string]string{}
	created := []string{}
	restore := func(cause error) error {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var failures []error
		for _, name := range created {
			if err := r.dockerRun(restoreCtx, svc, workDir, "rm", "-f", name); err != nil {
				failures = append(failures, err)
			}
		}
		for name, backup := range backups {
			if err := r.dockerRun(restoreCtx, svc, workDir, "rename", backup, name); err != nil {
				failures = append(failures, err)
				continue
			}
			if err := r.dockerRun(restoreCtx, svc, workDir, "start", name); err != nil {
				failures = append(failures, err)
			}
		}
		return errors.Join(append([]error{cause}, failures...)...)
	}
	for _, name := range old {
		if err := r.dockerRun(ctx, svc, workDir, "stop", name); err != nil {
			return restore(err)
		}
		backup := fmt.Sprintf("%s-previous-%d", name, time.Now().UnixNano())
		if err := r.dockerRun(ctx, svc, workDir, "rename", name, backup); err != nil {
			_ = r.dockerRun(context.Background(), svc, workDir, "start", name)
			return restore(err)
		}
		backups[name] = backup
	}
	replicas := svc.Replicas
	if replicas < 1 {
		replicas = 1
	}
	maxSize, maxFile := "50m", "3"
	if svc.Logging != nil {
		if svc.Logging.MaxSize != "" {
			maxSize = svc.Logging.MaxSize
		}
		if svc.Logging.MaxFile > 0 {
			maxFile = strconv.Itoa(svc.Logging.MaxFile)
		}
	}
	var upstreams []string
	for i := 1; i <= replicas; i++ {
		name := "kizuna-" + svc.Name
		if replicas > 1 {
			name += "-replica-" + strconv.Itoa(i)
		}
		args := []string{"run", "-d", "--name", name, "--label", "kizuna.service=" + svc.Name, "--restart", "unless-stopped", "--log-opt", "max-size=" + maxSize, "--log-opt", "max-file=" + maxFile}
		for _, port := range svc.Ports {
			if replicas > 1 {
				parts := strings.Split(port, ":")
				hostIndex := len(parts) - 2
				if hostIndex < 0 {
					return restore(fmt.Errorf("replicas require an explicit host port"))
				}
				hostPort, err := strconv.Atoi(parts[hostIndex])
				if err != nil || hostPort+i-1 > 65535 {
					return restore(fmt.Errorf("invalid replica port: %s", port))
				}
				parts[hostIndex] = strconv.Itoa(hostPort + i - 1)
				port = strings.Join(parts, ":")
				if len(upstreams) < i {
					upstreams = append(upstreams, fmt.Sprintf("127.0.0.1:%d", hostPort+i-1))
				}
			}
			args = append(args, "-p", port)
		}
		for k, v := range svc.Env {
			args = append(args, "-e", k+"="+v)
		}
		args = append(args, image)
		created = append(created, name)
		if err := r.dockerRun(ctx, svc, workDir, args...); err != nil {
			return restore(err)
		}
		if err := waitContainerReady(ctx, name); err != nil {
			return restore(err)
		}
	}
	svc.Image = image
	if svc.Ingress != nil && replicas > 1 && len(svc.Ingress.Upstreams) == 0 {
		svc.Ingress.Upstreams = upstreams
		if svc.Ingress.LBPolicy == "" {
			svc.Ingress.LBPolicy = "round_robin"
		}
	}
	// Keep the complete old deployment available until the new metadata is durable.
	r.mu.Lock()
	svc.State = entity.StateRunning
	svc.UpdatedAt = time.Now()
	saveErr := r.saveServicesLocked()
	if saveErr != nil {
		svc.State = entity.StateDeploying
	}
	r.mu.Unlock()
	if saveErr != nil {
		return restore(fmt.Errorf("persist replacement state: %w", saveErr))
	}
	for _, backup := range backups {
		if err := r.dockerRun(ctx, svc, workDir, "rm", backup); err != nil {
			return fmt.Errorf("replacement running and persisted, old container cleanup failed: %w", err)
		}
	}

	return nil
}

func waitContainerReady(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var runningSince time.Time
	for {
		state, hasHealth, restarts, err := inspectContainerState(ctx, name)
		if err != nil {
			return err
		}
		if restarts > 0 {
			return fmt.Errorf("container %s restarted during startup", name)
		}
		delay := 200 * time.Millisecond
		if state == entity.StateRunning {
			if hasHealth {
				return nil
			}
			// Without a healthcheck, observe the process across a stability window so
			// immediate exits and restart loops cannot pass on their first running tick.
			if runningSince.IsZero() {
				runningSince = time.Now()
			}
			elapsed := time.Since(runningSince)
			if elapsed >= time.Second {
				return nil
			}
			delay = time.Second - elapsed
		} else if state != entity.StateDeploying {
			return fmt.Errorf("container %s is not ready (%s)", name, state)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("container %s readiness: %w", name, ctx.Err())
		case <-time.After(delay):
		}
	}
}

func containerState(ctx context.Context, name string) (entity.ServiceState, error) {
	state, _, _, err := inspectContainerState(ctx, name)
	return state, err
}

func inspectContainerState(ctx context.Context, name string) (entity.ServiceState, bool, int, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", `{"State":{{json .State}},"RestartCount":{{.RestartCount}}}`, name).Output()
	if err != nil {
		return entity.StateFailed, false, 0, fmt.Errorf("inspect container %s: %w", name, err)
	}
	var snapshot struct {
		RestartCount int
		State        struct {
			Running    bool
			Restarting bool
			Status     string
			ExitCode   int
			Health     *struct{ Status string }
		}
	}
	if err := json.Unmarshal(out, &snapshot); err != nil {
		return entity.StateFailed, false, 0, err
	}
	state := snapshot.State
	hasHealth := state.Health != nil
	result := entity.StateRunning
	if state.Restarting || state.Status == "restarting" {
		result = entity.StateFailed
	} else if !state.Running {
		if state.ExitCode == 0 && (state.Status == "exited" || state.Status == "created") {
			result = entity.StateStopped
		} else {
			result = entity.StateFailed
		}
	} else if hasHealth {
		switch state.Health.Status {
		case "starting":
			result = entity.StateDeploying
		case "healthy":
		default:
			result = entity.StateFailed
		}
	}
	return result, hasHealth, snapshot.RestartCount, nil
}

func (r *Runner) deployProcess(ctx context.Context, svc *entity.Service, workDir string) error {
	r.mu.RLock()
	existing, done := r.processes[svc.Name], r.processDone[svc.Name]
	r.mu.RUnlock()
	if existing != nil && existing.Process != nil {
		if err := existing.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

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

	parts, err := shlex.Split(cmdStr, true)
	if err != nil {
		return fmt.Errorf("parse process command: %w", err)
	}
	if len(parts) == 0 {
		return fmt.Errorf("empty process command")
	}
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
		if c, ok := logWriter.(io.Closer); ok {
			_ = c.Close()
		}
		return fmt.Errorf("failed to start process: %w", err)
	}

	r.mu.Lock()
	r.processes[svc.Name] = cmd
	done = make(chan struct{})
	r.processDone[svc.Name] = done
	r.mu.Unlock()

	go func() {
		defer close(done)
		waitErr := cmd.Wait()
		if c, ok := logWriter.(io.Closer); ok {
			_ = c.Close()
		}
		r.mu.Lock()
		if r.processes[svc.Name] == cmd {
			delete(r.processes, svc.Name)
			delete(r.processDone, svc.Name)
			if cur, ok := r.services[svc.Name]; ok {
				cur.State = entity.StateStopped
				if waitErr != nil {
					cur.State = entity.StateFailed
				}
				cur.UpdatedAt = time.Now()
				_ = r.saveServicesLocked()
			}
		}
		r.mu.Unlock()
	}()

	return nil
}

// Stop terminates a running service
func (r *Runner) Stop(ctx context.Context, serviceName string) error {
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	r.mu.RLock()
	svc, ok := r.services[serviceName]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("service %q not found", serviceName)
	}
	var err error
	switch svc.Type {
	case entity.TypeCompose:
		dir := filepath.Join(r.baseDir, svc.Name)
		args := append(r.composeArgs(svc, dir), "stop")
		args = append(args, selectedServices(svc)...)
		err = r.dockerRun(ctx, svc, dir, args...)
	case entity.TypeDocker:
		names, listErr := r.managedContainers(ctx, serviceName)
		if listErr != nil {
			return listErr
		}
		for _, name := range names {
			if stopErr := r.dockerRun(ctx, svc, r.baseDir, "stop", name); stopErr != nil {
				err = errors.Join(err, stopErr)
			}
		}
	default:
		r.mu.RLock()
		p := r.processes[serviceName]
		done := r.processDone[serviceName]
		r.mu.RUnlock()
		if p != nil && p.Process != nil {
			err = p.Process.Kill()
			if errors.Is(err, os.ErrProcessDone) {
				err = nil
			}
			if err == nil && done != nil {
				select {
				case <-done:
				case <-ctx.Done():
					err = ctx.Err()
				}
			}
		}
	}
	if err != nil {
		return err
	}
	return r.updateState(serviceName, entity.StateStopped)
}

func (r *Runner) GetStatus(ctx context.Context, serviceName string) (*entity.Service, error) {
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if r.initErr != nil {
		return nil, r.initErr
	}
	r.mu.RLock()
	svc, ok := r.services[serviceName]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("service %q not found", serviceName)
	}
	state := entity.StateStopped
	var names []string
	var err error
	switch svc.Type {
	case entity.TypeDocker:
		names, err = r.managedContainers(ctx, serviceName)
	case entity.TypeCompose:
		args := append(r.composeArgs(svc, filepath.Join(r.baseDir, svc.Name)), "ps", "--all", "--quiet")
		args = append(args, selectedServices(svc)...)
		var out []byte
		out, err = exec.CommandContext(ctx, "docker", args...).Output()
		names = strings.Fields(string(out))
	default:
		r.mu.RLock()
		if svc.State == entity.StateFailed {
			state = entity.StateFailed
		}
		if r.processes[serviceName] != nil {
			state = entity.StateRunning
		}
		r.mu.RUnlock()
	}
	if err != nil {
		return nil, err
	}
	if len(names) > 0 {
		state = entity.StateRunning
		for _, name := range names {
			actual, e := containerState(ctx, name)
			if e != nil {
				return nil, e
			}
			if actual != entity.StateRunning {
				state = actual
				break
			}
		}
	}
	if svc.Type == entity.TypeDocker && svc.Replicas > len(names) {
		state = entity.StateFailed
	}
	if err := r.updateState(serviceName, state); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	clone := *svc
	return &clone, nil
}

func (r *Runner) ListServices(ctx context.Context) ([]*entity.Service, error) {
	r.mu.RLock()
	names := make([]string, 0, len(r.services))
	for name := range r.services {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	list := make([]*entity.Service, 0, len(names))
	for _, name := range names {
		svc, err := r.GetStatus(ctx, name)
		if err != nil {
			return nil, err
		}
		list = append(list, svc)
	}
	return list, nil
}

func (r *Runner) StreamLogs(ctx context.Context, serviceName string, writer io.Writer) error {
	if !validServiceName(serviceName) {
		return fmt.Errorf("invalid service name")
	}
	r.mu.RLock()
	svc, ok := r.services[serviceName]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("service %q not found", serviceName)
	}
	if svc.Type == entity.TypeCompose {
		args := append(r.composeArgs(svc, filepath.Join(r.baseDir, svc.Name)), "logs", "--follow", "--tail", "100")
		args = append(args, selectedServices(svc)...)
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdout, cmd.Stderr = writer, writer
		return cmd.Run()
	}
	if svc.Type == entity.TypeDocker {
		names, err := r.managedContainers(ctx, serviceName)
		if err != nil {
			return err
		}
		// Serialize writes from independent replica log streams.
		output := &lockedWriter{writer: writer}
		var wg sync.WaitGroup
		errs := make(chan error, len(names))
		for _, name := range names {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				cmd := exec.CommandContext(ctx, "docker", "logs", "--follow", "--tail", "100", name)
				cmd.Stdout, cmd.Stderr = output, output
				errs <- cmd.Run()
			}(name)
		}
		wg.Wait()
		close(errs)
		var result error
		for err := range errs {
			result = errors.Join(result, err)
		}
		return result
	}
	f, err := os.Open(filepath.Join(r.baseDir, "logs", serviceName+".log"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		if _, err = io.Copy(writer, f); err != nil {
			return err
		}
		r.mu.RLock()
		running := r.processes[serviceName] != nil
		r.mu.RUnlock()
		if !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func (r *Runner) updateState(name string, state entity.ServiceState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.services[name]; ok {
		s.State = state
		s.UpdatedAt = time.Now()
		return r.saveServicesLocked()
	}
	return nil
}

func (r *Runner) loadServices() error {
	if r.servicesFile == "" {
		return nil
	}
	data, err := os.ReadFile(r.servicesFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &r.services); err != nil {
		return fmt.Errorf("load service state: %w", err)
	}
	if r.services == nil {
		return fmt.Errorf("stored service state must be an object")
	}
	for name, svc := range r.services {
		if !validServiceName(name) || svc == nil || svc.Name != name {
			return fmt.Errorf("invalid stored service name")
		}
	}
	return os.Chmod(r.servicesFile, 0600)
}

func (r *Runner) saveServicesLocked() error {
	if r.servicesFile == "" {
		return nil
	}
	data, err := json.MarshalIndent(r.services, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(r.servicesFile), ".services-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), r.servicesFile); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(r.servicesFile))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func validServiceName(name string) bool { return entity.ValidateServiceName(name) == nil }

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
		if idx := strings.LastIndex(baseName, "-replica-"); idx != -1 {
			baseName = baseName[:idx]
		} else if idx := strings.LastIndex(baseName, "-"); idx > 0 {
			if _, exact := r.services[baseName]; !exact {
				if _, err := strconv.Atoi(baseName[idx+1:]); err == nil {
					if _, known := r.services[baseName[:idx]]; known {
						baseName = baseName[:idx]
					}
				}
			}
		}
		if strings.Contains(baseName, "-previous-") || !validServiceName(baseName) {
			continue
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

	if err := r.saveServicesLocked(); err != nil {
		r.initErr = err
	}
}

func (r *Runner) getLogWriter(serviceName string) io.Writer {
	logFile := filepath.Join(r.baseDir, "logs", fmt.Sprintf("%s.log", serviceName))
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return failingWriter{err}
	}
	return f
}

func unpackTarGz(r io.Reader, destDir string) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	cleanDest := filepath.Clean(destDir)
	rootInfo, err := os.Lstat(cleanDest)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive destination must be a directory without symlinks")
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		cleanName := filepath.Clean(header.Name)
		// Security: prevent ZipSlip / TarSlip path traversal attacks
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			continue
		}

		targetPath := filepath.Join(cleanDest, cleanName)
		if !strings.HasPrefix(targetPath, cleanDest+string(filepath.Separator)) && targetPath != cleanDest {
			continue
		}

		rel, err := filepath.Rel(cleanDest, targetPath)
		if err != nil {
			return err
		}
		check := cleanDest
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			check = filepath.Join(check, part)
			st, err := os.Lstat(check)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if err == nil && st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive path crosses symlink: %s", header.Name)
			}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return err
			}
			mode := header.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0644
			}
			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				return errors.Join(err, f.Close())
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive links are not supported: %s", header.Name)
		}
	}
	return nil
}

type failingWriter struct{ err error }

func (w failingWriter) Write(p []byte) (int, error) { return 0, w.err }
