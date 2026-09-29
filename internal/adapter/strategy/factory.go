package strategy

import (
	"fmt"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// Factory instantiates the appropriate TargetDeploymentStrategy based on target host properties
type Factory struct {
	localStrategy domain.TargetDeploymentStrategy
	meshStrategy  domain.TargetDeploymentStrategy
	sshStrategy   domain.TargetDeploymentStrategy
}

// NewFactory creates a new strategy factory with all registered deployment strategies
func NewFactory(local domain.TargetDeploymentStrategy, mesh domain.TargetDeploymentStrategy, ssh domain.TargetDeploymentStrategy) *Factory {
	if ssh == nil {
		ssh = NewSSHStrategy()
	}
	return &Factory{
		localStrategy: local,
		meshStrategy:  mesh,
		sshStrategy:   ssh,
	}
}

// GetStrategy resolves the appropriate DeploymentStrategy for a given TargetHost
func (f *Factory) GetStrategy(target *entity.TargetHost) (domain.TargetDeploymentStrategy, error) {
	if target == nil {
		return nil, fmt.Errorf("target host cannot be nil")
	}

	switch target.Type {
	case entity.TargetTypeLocal:
		if f.localStrategy != nil {
			return f.localStrategy, nil
		}
		return nil, fmt.Errorf("local deployment strategy not configured")

	case entity.TargetTypeSSH:
		if f.sshStrategy != nil {
			return f.sshStrategy, nil
		}
		return nil, fmt.Errorf("ssh deployment strategy not configured")

	case entity.TargetTypeMesh:
		if f.meshStrategy != nil {
			return f.meshStrategy, nil
		}
		return nil, fmt.Errorf("mesh deployment strategy not configured")

	default:
		// Default fallback to SSH for agentless if contains IP or host, otherwise Mesh
		if f.sshStrategy != nil {
			return f.sshStrategy, nil
		}
		return nil, fmt.Errorf("no matching deployment strategy for target type: %s", target.Type)
	}
}
