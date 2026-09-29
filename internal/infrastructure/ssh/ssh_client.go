package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Client provides agentless SSH execution and deployment capabilities
type Client struct {
	target *entity.TargetHost
}

// NewClient creates a new agentless SSH client for a target
func NewClient(target *entity.TargetHost) *Client {
	return &Client{target: target}
}

// getAuthMethods discovers available SSH auth mechanisms (agent, keys, password)
func (c *Client) getAuthMethods() []ssh.AuthMethod {
	var methods []ssh.AuthMethod

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

	return methods
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

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            c.getAuthMethods(),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // In production can use known_hosts
		Timeout:         15 * time.Second,
	}

	// Dial with context
	dialer := net.Dialer{Timeout: config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh dial to %s failed: %w", addr, err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake with %s failed: %w", addr, err)
	}

	return ssh.NewClient(sshConn, chans, reqs), nil
}

// Run executes a command string on the remote host and returns stdout/stderr
func (c *Client) Run(ctx context.Context, cmdStr string) (string, error) {
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
	session.Stderr = &outBuf

	err = session.Run(cmdStr)
	return strings.TrimSpace(outBuf.String()), err
}

// StreamToCommand executes a command remotely while piping stdinReader into the remote command's stdin
func (c *Client) StreamToCommand(ctx context.Context, cmdStr string, stdinReader io.Reader) (string, error) {
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
	session.Stderr = &outBuf

	err = session.Run(cmdStr)
	return strings.TrimSpace(outBuf.String()), err
}

// DeployContainer performs agentless deployment of a container workload on target
func (c *Client) DeployContainer(ctx context.Context, svc *entity.Service, artifactReader io.Reader) (*entity.DeployResult, error) {
	start := time.Now()
	containerName := fmt.Sprintf("kizuna-%s", svc.Name)

	// 1. Verify Docker is available on the remote host
	if _, err := c.Run(ctx, "docker --version"); err != nil {
		return &entity.DeployResult{
			Target:      c.target.Address(),
			ServiceName: svc.Name,
			Success:     false,
			Duration:    time.Since(start),
			Error:       fmt.Sprintf("docker is not installed or accessible on target: %v", err),
		}, fmt.Errorf("docker not available on target %s: %w", c.target.Address(), err)
	}

	// 2. Prepare port flags and environment flags
	var portFlags []string
	for _, p := range svc.Ports {
		portFlags = append(portFlags, fmt.Sprintf("-p %s", p))
	}
	var envFlags []string
	for k, v := range svc.Env {
		envFlags = append(envFlags, fmt.Sprintf("-e %s=%q", k, v))
	}

	// 3. Build or Pull image
	imageName := svc.Image
	if imageName == "" {
		imageName = fmt.Sprintf("kizuna-%s:latest", svc.Name)
		remoteDir := fmt.Sprintf("/tmp/kizuna/%s", svc.Name)

		// Create remote staging directory
		if _, err := c.Run(ctx, fmt.Sprintf("mkdir -p %s", remoteDir)); err != nil {
			return nil, fmt.Errorf("failed to create remote directory %s: %w", remoteDir, err)
		}

		// Transfer and unpack artifact tarball if provided
		if artifactReader != nil {
			extractCmd := fmt.Sprintf("tar -xzf - -C %s", remoteDir)
			if _, err := c.StreamToCommand(ctx, extractCmd, artifactReader); err != nil {
				return nil, fmt.Errorf("failed to transfer artifact to remote %s: %w", remoteDir, err)
			}
		}

		// Build image on remote machine
		buildCmd := fmt.Sprintf("cd %s && docker build -t %s .", remoteDir, imageName)
		if out, err := c.Run(ctx, buildCmd); err != nil {
			return nil, fmt.Errorf("failed to build docker image on remote: %v (output: %s)", err, out)
		}
	} else {
		// Pull prebuilt image
		if out, err := c.Run(ctx, fmt.Sprintf("docker pull %s", imageName)); err != nil {
			return nil, fmt.Errorf("failed to pull image %s on remote: %v (output: %s)", imageName, err, out)
		}
	}

	// 4. Stop and remove existing container
	_ , _ = c.Run(ctx, fmt.Sprintf("docker stop %s 2>/dev/null || true", containerName))
	_ , _ = c.Run(ctx, fmt.Sprintf("docker rm %s 2>/dev/null || true", containerName))

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

	// 5. Run new container with bounded log rotation
	runCmd := fmt.Sprintf("docker run -d --name %s --restart unless-stopped --log-opt max-size=%s --log-opt max-file=%s %s %s %s",
		containerName,
		maxSize,
		maxFile,
		strings.Join(portFlags, " "),
		strings.Join(envFlags, " "),
		imageName,
	)

	containerID, err := c.Run(ctx, runCmd)
	if err != nil {
		return &entity.DeployResult{
			Target:      c.target.Address(),
			ServiceName: svc.Name,
			Success:     false,
			Duration:    time.Since(start),
			Error:       fmt.Sprintf("failed to run container: %v (output: %s)", err, containerID),
		}, err
	}

	// 6. Inspect container status
	status, _ := c.Run(ctx, fmt.Sprintf("docker inspect -f '{{.State.Status}}' %s", containerName))
	if status != "running" {
		logs, _ := c.Run(ctx, fmt.Sprintf("docker logs --tail 20 %s", containerName))
		return &entity.DeployResult{
			Target:      c.target.Address(),
			ServiceName: svc.Name,
			Success:     false,
			Duration:    time.Since(start),
			ContainerID: containerID,
			Error:       fmt.Sprintf("container not running (status: %s, logs: %s)", status, logs),
		}, fmt.Errorf("container exited with status: %s", status)
	}

	return &entity.DeployResult{
		Target:      c.target.Address(),
		ServiceName: svc.Name,
		Success:     true,
		Duration:    time.Since(start),
		ContainerID: strings.TrimSpace(containerID),
	}, nil
}
