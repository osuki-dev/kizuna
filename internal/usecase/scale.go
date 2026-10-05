package usecase

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/i18n"
)

// ScaleUseCase handles one-click horizontal scaling and load balancer synchronization
type ScaleUseCase struct {
	deployUC *DeployUseCase
}

// NewScaleUseCase initializes ScaleUseCase
func NewScaleUseCase(deployUC *DeployUseCase) *ScaleUseCase {
	return &ScaleUseCase{deployUC: deployUC}
}

// Execute scales a given service to target replica count and reconfigures load balancing
func (uc *ScaleUseCase) Execute(ctx context.Context, project *entity.Project, serviceName string, replicas int, logWriter io.Writer) error {
	if logWriter == nil {
		logWriter = os.Stdout
	}

	if replicas <= 0 {
		return fmt.Errorf("replica count must be at least 1, got %d", replicas)
	}

	svc, ok := project.Services[serviceName]
	if !ok {
		return fmt.Errorf("service %q not found in configuration", serviceName)
	}
	original := svc
	svc, err := cloneService(svc)
	if err != nil {
		return err
	}
	copiedProject := *project
	copiedProject.Services = make(map[string]*entity.Service, len(project.Services))
	for name, service := range project.Services {
		copiedProject.Services[name] = service
	}
	copiedProject.Services[serviceName] = svc

	_, _ = fmt.Fprintln(logWriter, i18n.T("scale_starting", serviceName, replicas))

	// Update service replicas
	svc.Replicas = replicas

	// Execute deployment with updated replicas
	if err := uc.deployUC.Execute(ctx, &copiedProject, serviceName, logWriter); err != nil {
		_, _ = fmt.Fprintln(logWriter, i18n.T("scale_failed", serviceName, err))
		return err
	}

	*original = *svc
	upstreamsStr := "-"
	if svc.Ingress != nil && len(svc.Ingress.Upstreams) > 0 {
		upstreamsStr = strings.Join(svc.Ingress.Upstreams, ", ")
	}

	_, _ = fmt.Fprintln(logWriter, i18n.T("scale_success", serviceName, replicas, upstreamsStr))
	return nil
}
