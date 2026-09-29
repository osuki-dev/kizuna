package strategy

import (
	"context"
	"io"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// LocalStrategy implements domain.TargetDeploymentStrategy for local machine deployments
type LocalStrategy struct {
	runner domain.WorkloadRunner
}

// NewLocalStrategy creates a new LocalStrategy
func NewLocalStrategy(runner domain.WorkloadRunner) domain.TargetDeploymentStrategy {
	return &LocalStrategy{runner: runner}
}

// Protocol returns TargetTypeLocal
func (s *LocalStrategy) Protocol() entity.TargetType {
	return entity.TargetTypeLocal
}

// Deploy deploys service locally
func (s *LocalStrategy) Deploy(ctx context.Context, target *entity.TargetHost, svc *entity.Service, artifact io.Reader) (*entity.DeployResult, error) {
	start := time.Now()
	err := s.runner.Deploy(ctx, svc, artifact)
	if err != nil {
		return &entity.DeployResult{
			Target:      "localhost",
			ServiceName: svc.Name,
			Success:     false,
			Duration:    time.Since(start),
			Error:       err.Error(),
		}, err
	}

	return &entity.DeployResult{
		Target:      "localhost",
		ServiceName: svc.Name,
		Success:     true,
		Duration:    time.Since(start),
	}, nil
}

// Stop stops the local service
func (s *LocalStrategy) Stop(ctx context.Context, target *entity.TargetHost, serviceName string) error {
	return s.runner.Stop(ctx, serviceName)
}

// GetStatus retrieves local service status
func (s *LocalStrategy) GetStatus(ctx context.Context, target *entity.TargetHost, serviceName string) (*entity.Service, error) {
	return s.runner.GetStatus(ctx, serviceName)
}
