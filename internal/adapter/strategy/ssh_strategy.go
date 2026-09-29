package strategy

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/ssh"
)

// SSHStrategy implements domain.TargetDeploymentStrategy for agentless SSH deployments
type SSHStrategy struct{}

// NewSSHStrategy creates a new SSHStrategy instance
func NewSSHStrategy() domain.TargetDeploymentStrategy {
	return &SSHStrategy{}
}

// Protocol returns TargetTypeSSH
func (s *SSHStrategy) Protocol() entity.TargetType {
	return entity.TargetTypeSSH
}

// Deploy executes agentless deployment on target server via SSH
func (s *SSHStrategy) Deploy(ctx context.Context, target *entity.TargetHost, svc *entity.Service, artifact io.Reader) (*entity.DeployResult, error) {
	client := ssh.NewClient(target)
	return client.DeployContainer(ctx, svc, artifact)
}

// Stop stops the service container via SSH
func (s *SSHStrategy) Stop(ctx context.Context, target *entity.TargetHost, serviceName string) error {
	client := ssh.NewClient(target)
	containerName := fmt.Sprintf("kizuna-%s", serviceName)
	_, err := client.Run(ctx, fmt.Sprintf("docker stop %s", containerName))
	return err
}

// GetStatus checks remote container status via SSH
func (s *SSHStrategy) GetStatus(ctx context.Context, target *entity.TargetHost, serviceName string) (*entity.Service, error) {
	client := ssh.NewClient(target)
	containerName := fmt.Sprintf("kizuna-%s", serviceName)
	status, err := client.Run(ctx, fmt.Sprintf("docker inspect -f '{{.State.Status}}' %s", containerName))
	if err != nil {
		return &entity.Service{
			Name:      serviceName,
			State:     entity.StateStopped,
			UpdatedAt: time.Now(),
		}, nil
	}

	state := entity.StateStopped
	if strings.TrimSpace(status) == "running" {
		state = entity.StateRunning
	}

	return &entity.Service{
		Name:      serviceName,
		State:     state,
		UpdatedAt: time.Now(),
	}, nil
}
