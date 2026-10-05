package strategy

import (
	"context"
	"io"

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
	return client.Deploy(ctx, svc, artifact)
}

// Stop stops the service container via SSH
func (s *SSHStrategy) Stop(ctx context.Context, target *entity.TargetHost, serviceName string) error {
	client := ssh.NewClient(target)
	return client.Stop(ctx, serviceName)
}

// GetStatus checks remote container status via SSH
func (s *SSHStrategy) GetStatus(ctx context.Context, target *entity.TargetHost, serviceName string) (*entity.Service, error) {
	client := ssh.NewClient(target)
	return client.GetStatus(ctx, serviceName)
}
