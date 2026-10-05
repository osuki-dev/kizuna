package ssh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Client provides agentless SSH execution and deployment capabilities
type Client struct {
	target        *entity.TargetHost
	runCommand    func(context.Context, string) (string, error)
	streamCommand func(context.Context, string, io.Reader) (string, error)
}

// NewClient creates a new agentless SSH client for a target
func NewClient(target *entity.TargetHost) *Client {
	return &Client{target: target}
}

// getAuthMethods discovers available SSH auth mechanisms (agent, keys, password)
func (c *Client) getAuthMethods() ([]ssh.AuthMethod, func()) {
	var methods []ssh.AuthMethod
	var closers []func()
	cleanup := func() {
		for _, cl := range closers {
			cl()
		}
	}

	// 1. Password if provided
	if c.target.Password != "" {
		methods = append(methods, ssh.Password(c.target.Password))
	}

	// 2. Explicit key path
	if c.target.KeyPath != "" {
		if keyBytes, err := os.ReadFile(c.target.KeyPath); err == nil {
			if signer, err := ssh.ParsePrivateKey(keyBytes); err == nil {
				methods = append(methods, ssh.PublicKeys(signer))
			}
		}
	}

	// 3. System SSH agent
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			agentClient := agent.NewClient(conn)
			if signers, err := agentClient.Signers(); err == nil && len(signers) > 0 {
				methods = append(methods, ssh.PublicKeys(signers...))
				closers = append(closers, func() { _ = conn.Close() })
			} else {
				_ = conn.Close()
			}
		}
	}

	// 4. Default user SSH keys (~/.ssh/id_ed25519, ~/.ssh/id_rsa)
	home, _ := os.UserHomeDir()
	if home != "" {
		for _, keyName := range []string{"id_ed25519", "id_rsa"} {
			keyPath := filepath.Join(home, ".ssh", keyName)
			if keyBytes, err := os.ReadFile(keyPath); err == nil {
				if signer, err := ssh.ParsePrivateKey(keyBytes); err == nil {
					methods = append(methods, ssh.PublicKeys(signer))
				}
			}
		}
	}

	return methods, cleanup
}

// connect establishes an SSH connection to the target host
func (c *Client) connect(ctx context.Context) (*ssh.Client, error) {
	user := c.target.User
	if user == "" {
		user = os.Getenv("USER")
		if user == "" {
			user = "root"
		}
	}

	port := c.target.Port
	if port <= 0 {
		port = 22
	}

	addr := net.JoinHostPort(c.target.Host, strconv.Itoa(port))

	authMethods, cleanup := c.getAuthMethods()
	defer cleanup()

	hostKeyCallback, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}

	// Dial with context
	dialer := net.Dialer{Timeout: config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh dial to %s failed: %w", addr, err)
	}

	// ssh.NewClientConn does not honor ClientConfig.Timeout or a context.
	// Bound banner exchange and authentication as well as the initial TCP dial.
	handshakeDeadline := time.Now().Add(config.Timeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	if err := conn.SetDeadline(handshakeDeadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set SSH handshake deadline: %w", err)
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	stopCancellation()
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake with %s canceled: %w", addr, ctx.Err())
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake with %s failed: %w", addr, err)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = sshConn.Close()
		return nil, fmt.Errorf("clear SSH handshake deadline: %w", err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// Run executes a command string and returns stdout; stderr is not mixed into machine-readable output.
func (c *Client) Run(ctx context.Context, cmdStr string) (string, error) {
	if c.runCommand != nil {
		return c.runCommand(ctx, cmdStr)
	}
	client, err := c.connect(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to open ssh session: %w", err)
	}
	defer func() { _ = session.Close() }()

	var outBuf bytes.Buffer
	session.Stdout = &outBuf
	session.Stderr = io.Discard

	err = runSession(ctx, session, cmdStr)
	return strings.TrimSpace(outBuf.String()), err
}

// StreamToCommand executes a command remotely while piping stdinReader into the remote command's stdin
func (c *Client) StreamToCommand(ctx context.Context, cmdStr string, stdinReader io.Reader) (string, error) {
	if c.streamCommand != nil {
		return c.streamCommand(ctx, cmdStr, stdinReader)
	}
	client, err := c.connect(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to open ssh session: %w", err)
	}
	defer func() { _ = session.Close() }()

	session.Stdin = stdinReader
	var outBuf bytes.Buffer
	session.Stdout = &outBuf
	session.Stderr = io.Discard

	err = runSession(ctx, session, cmdStr)
	return strings.TrimSpace(outBuf.String()), err
}

// hostKeyCallback uses the user's existing trust store. Unknown or changed keys fail closed.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate SSH known_hosts: %w", err)
	}
	var files []string
	for _, file := range []string{filepath.Join(home, ".ssh", "known_hosts"), "/etc/ssh/ssh_known_hosts"} {
		if _, err := os.Stat(file); err == nil {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("SSH known_hosts is missing; verify the target host key and add it to ~/.ssh/known_hosts before deploying")
	}
	callback, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("load SSH known_hosts: %w", err)
	}
	return callback, nil
}

func runSession(ctx context.Context, session *ssh.Session, command string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := session.Start(command); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = session.Close()
		<-done
		return ctx.Err()
	}
}

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func command(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = quote(arg)
	}
	return strings.Join(quoted, " ")
}

var serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func validateService(svc *entity.Service) error {
	if svc == nil || !serviceNamePattern.MatchString(svc.Name) {
		return fmt.Errorf("invalid service name")
	}
	if svc.Replicas > 1 {
		return fmt.Errorf("SSH deployment does not support replicas greater than one")
	}
	return nil
}
func descriptorPath(name string) string {
	return `"$HOME/.local/share/kizuna/services/` + name + `.json"`
}
func (c *Client) saveService(ctx context.Context, svc *entity.Service) error {
	// Runtime metadata must not persist environment secrets.
	copy := *svc
	copy.Env, copy.Backup, copy.Ingress = nil, nil, nil
	if copy.Deploy != nil {
		deploy := *copy.Deploy
		deploy.Env = nil
		copy.Deploy = &deploy
	}
	data, err := json.Marshal(&copy)
	if err != nil {
		return err
	}
	_, err = c.StreamToCommand(ctx, `umask 077; mkdir -p "$HOME/.local/share/kizuna/services" && cat > `+descriptorPath(svc.Name)+`.tmp && mv `+descriptorPath(svc.Name)+`.tmp `+descriptorPath(svc.Name), bytes.NewReader(data))
	return err
}
func (c *Client) loadService(ctx context.Context, name string) (*entity.Service, error) {
	if !serviceNamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid service name")
	}
	output, err := c.Run(ctx, "cat "+descriptorPath(name))
	if err != nil {
		return nil, fmt.Errorf("read remote service metadata: %w", err)
	}
	var svc entity.Service
	if err := json.Unmarshal([]byte(output), &svc); err != nil {
		return nil, fmt.Errorf("decode remote service metadata: %w", err)
	}
	if svc.Name != name {
		return nil, fmt.Errorf("remote service metadata name mismatch")
	}
	return &svc, nil
}
func (c *Client) Deploy(ctx context.Context, svc *entity.Service, artifact io.Reader) (*entity.DeployResult, error) {
	if err := validateService(svc); err != nil {
		return nil, err
	}
	switch svc.Type {
	case entity.TypeDocker:
		return c.DeployContainer(ctx, svc, artifact)
	case entity.TypeCompose:
		return c.deployCompose(ctx, svc, artifact)
	default:
		return nil, fmt.Errorf("SSH deployment does not support workload type %q", svc.Type)
	}
}

func (c *Client) prepareDirectory(ctx context.Context, svc *entity.Service, artifact io.Reader) (string, error) {
	dir := ""
	if svc.Deploy != nil {
		dir = svc.Deploy.Dest
	}
	if dir == "" {
		if artifact == nil {
			return "", fmt.Errorf("remote deployment requires an artifact or deploy.dest")
		}
		home, err := c.Run(ctx, `printf '%s' "$HOME"`)
		if err != nil {
			return "", fmt.Errorf("resolve remote home directory: %w", err)
		}
		if home == "" {
			return "", fmt.Errorf("remote home directory is empty")
		}
		dir = filepath.Join(home, ".local", "share", "kizuna", "workloads", svc.Name)
	}
	if _, err := c.Run(ctx, command("mkdir", "-p", "--", dir)); err != nil {
		return "", err
	}
	if artifact != nil {
		if _, err := c.StreamToCommand(ctx, command("tar", "-xzf", "-", "--no-same-owner", "-C", dir), artifact); err != nil {
			return "", fmt.Errorf("transfer workload artifact: %w", err)
		}
	}
	return dir, nil
}

// DeployContainer retains the old container until the replacement starts successfully.
func (c *Client) DeployContainer(ctx context.Context, svc *entity.Service, artifact io.Reader) (*entity.DeployResult, error) {
	start := time.Now()
	if err := validateService(svc); err != nil {
		return nil, err
	}
	fail := func(err error) (*entity.DeployResult, error) {
		return &entity.DeployResult{Target: c.target.Address(), ServiceName: svc.Name, Duration: time.Since(start), Error: err.Error()}, err
	}
	if _, err := c.Run(ctx, command("docker", "--version")); err != nil {
		return fail(fmt.Errorf("docker unavailable: %w", err))
	}
	image := svc.Image
	if image == "" || svc.Dockerfile != "" {
		dir, err := c.prepareDirectory(ctx, svc, artifact)
		if err != nil {
			return fail(err)
		}
		image = "kizuna-" + svc.Name + ":latest"
		args := []string{"docker", "build", "-t", image}
		if svc.Dockerfile != "" {
			file := svc.Dockerfile
			if filepath.IsAbs(file) && artifact != nil {
				rel, err := filepath.Rel(svc.Root, file)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return fail(fmt.Errorf("dockerfile must be inside uploaded service root"))
				}
				file = rel
			}
			if !filepath.IsAbs(file) {
				file = filepath.Join(dir, file)
			}
			args = append(args, "-f", file)
		}
		args = append(args, dir)
		if _, err := c.Run(ctx, command(args...)); err != nil {
			return fail(fmt.Errorf("build image: %w", err))
		}
	} else if !strings.HasPrefix(image, "sha256:") {
		if _, err := c.Run(ctx, command("docker", "pull", image)); err != nil {
			return fail(fmt.Errorf("pull image: %w", err))
		}
	}
	imageID, err := c.Run(ctx, command("docker", "image", "inspect", "--format", "{{.Id}}", image))
	if err != nil {
		return fail(fmt.Errorf("resolve image ID: %w", err))
	}
	if !strings.HasPrefix(strings.TrimSpace(imageID), "sha256:") {
		return fail(fmt.Errorf("remote Docker returned an invalid image ID"))
	}
	imageID = strings.TrimSpace(imageID)
	name := "kizuna-" + svc.Name
	old, err := c.Run(ctx, command("docker", "ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}"))
	if err != nil {
		return fail(fmt.Errorf("inspect existing container: %w", err))
	}
	backup := name + "-previous-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	hadOld := strings.TrimSpace(old) != ""
	oldRunning := false
	if hadOld {
		state, err := c.Run(ctx, command("docker", "inspect", "--format", "{{.State.Running}}", name))
		if err != nil {
			return fail(err)
		}
		oldRunning = strings.TrimSpace(state) == "true"
		if _, err := c.Run(ctx, command("docker", "stop", name)); err != nil {
			return fail(err)
		}
		if _, err := c.Run(ctx, command("docker", "rename", name, backup)); err != nil {
			if oldRunning {
				_, _ = c.Run(ctx, command("docker", "start", name))
			}
			return fail(err)
		}
	}
	restore := func(cause error) error {
		// Recovery must still run after the caller's deployment timeout.
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := c.Run(recoveryCtx, command("docker", "rm", "-f", name)); err != nil {
			cause = fmt.Errorf("%w; remove replacement: %v", cause, err)
		}
		if hadOld {
			if _, err := c.Run(recoveryCtx, command("docker", "rename", backup, name)); err != nil {
				return fmt.Errorf("%w; restore old container name: %v", cause, err)
			}
			if oldRunning {
				if _, err := c.Run(recoveryCtx, command("docker", "start", name)); err != nil {
					return fmt.Errorf("%w; restart old container: %v", cause, err)
				}
			}
		}
		return cause
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
	args := []string{"docker", "run", "-d", "--name", name, "--restart", "unless-stopped", "--log-opt", "max-size=" + maxSize, "--log-opt", "max-file=" + maxFile}
	for _, port := range svc.Ports {
		args = append(args, "-p", port)
	}
	keys := make([]string, 0, len(svc.Env))
	for key := range svc.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "-e", key+"="+svc.Env[key])
	}
	args = append(args, imageID)
	id, err := c.Run(ctx, command(args...))
	if err != nil {
		return fail(restore(fmt.Errorf("start replacement container: %w", err)))
	}
	if err := c.waitContainer(ctx, name); err != nil {
		return fail(restore(err))
	}
	svc.Image = imageID
	svc.State = entity.StateRunning
	svc.UpdatedAt = time.Now()
	if err := c.saveService(ctx, svc); err != nil {
		return fail(restore(fmt.Errorf("persist remote metadata: %w", err)))
	}
	if hadOld {
		if _, err := c.Run(ctx, command("docker", "rm", backup)); err != nil {
			return fail(fmt.Errorf("replacement running but old container cleanup failed: %w", err))
		}
	}
	return &entity.DeployResult{Target: c.target.Address(), ServiceName: svc.Name, Success: true, Duration: time.Since(start), ContainerID: strings.TrimSpace(id)}, nil
}

func composeCommand(svc *entity.Service, operation string) (string, error) {
	file := svc.ComposeFile
	if file == "" {
		file = "docker-compose.yml"
	}
	args := []string{"docker", "compose", "-f", file}
	if svc.Compose != nil {
		if svc.Compose.ProjectName != "" {
			args = append(args, "--project-name", svc.Compose.ProjectName)
		} else {
			args = append(args, "--project-name", svc.Name)
		}
		for _, file := range svc.Compose.EnvFiles {
			args = append(args, "--env-file", file)
		}
		for _, profile := range svc.Compose.Profiles {
			args = append(args, "--profile", profile)
		}
	} else {
		args = append(args, "--project-name", svc.Name)
	}
	switch operation {
	case "up":
		args = append(args, "up", "-d", "--wait", "--wait-timeout", "60")
	case "stop":
		args = append(args, "stop")
	case "logs":
		args = append(args, "logs", "--follow", "--tail", "100")
	case "status":
		args = append(args, "ps", "-a", "--format", "json")
	default:
		return "", fmt.Errorf("unsupported Compose operation")
	}
	if svc.Compose != nil {
		for _, name := range svc.Compose.Services {
			if !serviceNamePattern.MatchString(name) {
				return "", fmt.Errorf("invalid Compose service name")
			}
			args = append(args, name)
		}
	}
	var env []string
	if operation == "up" {
		keys := make([]string, 0, len(svc.Env))
		for key := range svc.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(key) {
				return "", fmt.Errorf("invalid environment variable name")
			}
			env = append(env, key+"="+quote(svc.Env[key]))
		}
	}
	prefix := ""
	if len(env) > 0 {
		prefix = strings.Join(env, " ") + " "
	}
	loadEnv := ""
	if operation != "up" {
		file := `"$HOME/.local/share/kizuna/services/` + svc.Name + `.env"`
		loadEnv = "if [ -f " + file + " ]; then set -a; . " + file + "; set +a; fi; "
	}
	return loadEnv + "cd " + quote(svc.Root) + " && " + prefix + command(args...), nil
}
func (c *Client) deployCompose(ctx context.Context, svc *entity.Service, artifact io.Reader) (*entity.DeployResult, error) {
	start := time.Now()
	dir, err := c.prepareDirectory(ctx, svc, artifact)
	if err != nil {
		return nil, err
	}
	remote := *svc
	remote.Root = dir
	if filepath.IsAbs(svc.ComposeFile) && artifact != nil {
		rel, err := filepath.Rel(svc.Root, svc.ComposeFile)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("compose file must be inside the uploaded service root")
		}
		remote.ComposeFile = rel
	}
	cmd, err := composeCommand(&remote, "up")
	if err != nil {
		return nil, err
	}
	if _, err = c.Run(ctx, cmd); err != nil {
		return &entity.DeployResult{Target: c.target.Address(), ServiceName: svc.Name, Duration: time.Since(start), Error: err.Error()}, fmt.Errorf("remote Compose deployment failed: %w", err)
	}
	remote.State = entity.StateRunning
	remote.UpdatedAt = time.Now()
	if err = c.saveComposeEnv(ctx, &remote); err != nil {
		return nil, fmt.Errorf("persist remote Compose environment: %w", err)
	}
	if err = c.saveService(ctx, &remote); err != nil {
		return nil, fmt.Errorf("persist remote Compose metadata: %w", err)
	}
	svc.State = remote.State
	svc.UpdatedAt = remote.UpdatedAt
	return &entity.DeployResult{Target: c.target.Address(), ServiceName: svc.Name, Success: true, Duration: time.Since(start)}, nil
}
func (c *Client) Stop(ctx context.Context, name string) error {
	svc, err := c.loadService(ctx, name)
	if err != nil {
		return err
	}
	var cmd string
	switch svc.Type {
	case entity.TypeCompose:
		cmd, err = composeCommand(svc, "stop")
	case entity.TypeDocker:
		cmd = command("docker", "stop", "kizuna-"+name)
	default:
		return fmt.Errorf("unsupported remote workload type %q", svc.Type)
	}
	if err != nil {
		return err
	}
	_, err = c.Run(ctx, cmd)
	return err
}
func (c *Client) GetStatus(ctx context.Context, name string) (*entity.Service, error) {
	svc, err := c.loadService(ctx, name)
	if err != nil {
		return nil, err
	}
	svc.UpdatedAt = time.Now()
	svc.State = entity.StateStopped
	if svc.Type == entity.TypeCompose {
		cmd, err := composeCommand(svc, "status")
		if err != nil {
			return nil, err
		}
		out, err := c.Run(ctx, cmd)
		if err != nil {
			return nil, fmt.Errorf("query remote Compose status: %w", err)
		}
		type container struct {
			State  string
			Health string
		}
		var containers []container
		if strings.HasPrefix(strings.TrimSpace(out), "[") {
			if err = json.Unmarshal([]byte(out), &containers); err != nil {
				return nil, err
			}
		} else {
			decoder := json.NewDecoder(strings.NewReader(out))
			for {
				var row container
				err = decoder.Decode(&row)
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, err
				}
				containers = append(containers, row)
			}
		}
		if len(containers) > 0 {
			svc.State = entity.StateRunning
			for _, row := range containers {
				if row.Health == "unhealthy" {
					svc.State = entity.StateFailed
					break
				}
				if row.State != "running" {
					svc.State = entity.StateStopped
				}
			}
		}
		return svc, nil
	}
	if svc.Type != entity.TypeDocker {
		return nil, fmt.Errorf("unsupported remote workload type %q", svc.Type)
	}
	out, err := c.Run(ctx, command("docker", "inspect", "--format", "{{.State.Status}}", "kizuna-"+name))
	if err != nil {
		return nil, fmt.Errorf("query remote container status: %w", err)
	}
	if strings.TrimSpace(out) == "running" {
		svc.State = entity.StateRunning
	} else if strings.TrimSpace(out) == "dead" {
		svc.State = entity.StateFailed
	}
	return svc, nil
}

func (c *Client) waitContainer(ctx context.Context, name string) error {
	readyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var stableSince time.Time
	const inspectReadiness = `{"status":{{json .State.Status}},"health":{{if .State.Health}}{{json .State.Health.Status}}{{else}}""{{end}},"restarting":{{.State.Restarting}},"restart_count":{{.RestartCount}}}`
	for {
		output, err := c.Run(readyCtx, command("docker", "inspect", "--format", inspectReadiness, name))
		if err != nil {
			return fmt.Errorf("inspect replacement readiness: %w", err)
		}
		var state struct {
			Status       string `json:"status"`
			Health       string `json:"health"`
			Restarting   bool   `json:"restarting"`
			RestartCount int    `json:"restart_count"`
		}
		if err := json.Unmarshal([]byte(output), &state); err != nil {
			return fmt.Errorf("decode replacement readiness: %w", err)
		}
		if state.Status != "running" || state.Restarting || state.RestartCount > 0 {
			return fmt.Errorf("replacement container stopped or restarted during startup")
		}
		if state.Health == "healthy" {
			return nil
		}
		if state.Health == "unhealthy" {
			return fmt.Errorf("replacement container is unhealthy")
		}
		if state.Health == "" {
			if stableSince.IsZero() {
				stableSince = time.Now()
			} else if time.Since(stableSince) >= time.Second {
				return nil
			}
		} else {
			stableSince = time.Time{}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			return fmt.Errorf("wait for replacement readiness: %w", readyCtx.Err())
		case <-timer.C:
		}
	}
}

// StreamServiceLogs reads the runtime logs for an explicitly selected remote service.
func (c *Client) StreamServiceLogs(ctx context.Context, name string, writer io.Writer) error {
	svc, err := c.loadService(ctx, name)
	if err != nil {
		return err
	}
	cmd := command("docker", "logs", "--follow", "--tail", "100", "kizuna-"+name)
	if svc.Type == entity.TypeCompose {
		base, err := composeCommand(svc, "logs")
		if err != nil {
			return err
		}
		cmd = base
	} else if svc.Type != entity.TypeDocker {
		return fmt.Errorf("unsupported remote workload type %q", svc.Type)
	}
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	session.Stdout = writer
	session.Stderr = writer
	return runSession(ctx, session, cmd)
}

func (c *Client) saveComposeEnv(ctx context.Context, svc *entity.Service) error {
	keys := make([]string, 0, len(svc.Env))
	for key := range svc.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var data strings.Builder
	for _, key := range keys {
		data.WriteString(key + "=" + quote(svc.Env[key]) + "\n")
	}
	path := `"$HOME/.local/share/kizuna/services/` + svc.Name + `.env"`
	_, err := c.StreamToCommand(ctx, `umask 077; mkdir -p "$HOME/.local/share/kizuna/services" && cat > `+path+`.tmp && mv `+path+`.tmp `+path, strings.NewReader(data.String()))
	return err
}
