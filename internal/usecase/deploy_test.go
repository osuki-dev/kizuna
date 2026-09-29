package usecase_test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/usecase"
)

type mockStrategy struct {
	mu           sync.Mutex
	proto        entity.TargetType
	deployedHosts []string
}

func (m *mockStrategy) Protocol() entity.TargetType {
	return m.proto
}

func (m *mockStrategy) Deploy(ctx context.Context, target *entity.TargetHost, svc *entity.Service, artifact io.Reader) (*entity.DeployResult, error) {
	m.mu.Lock()
	m.deployedHosts = append(m.deployedHosts, target.Address())
	m.mu.Unlock()

	return &entity.DeployResult{
		Target:      target.Address(),
		ServiceName: svc.Name,
		Success:     true,
		Duration:    10 * time.Millisecond,
	}, nil
}

func (m *mockStrategy) Stop(ctx context.Context, target *entity.TargetHost, serviceName string) error {
	return nil
}

func (m *mockStrategy) GetStatus(ctx context.Context, target *entity.TargetHost, serviceName string) (*entity.Service, error) {
	return &entity.Service{Name: serviceName, State: entity.StateRunning}, nil
}

type mockStrategyResolver struct {
	strat domain.TargetDeploymentStrategy
}

func (r *mockStrategyResolver) GetStrategy(target *entity.TargetHost) (domain.TargetDeploymentStrategy, error) {
	return r.strat, nil
}

type mockIngressManager struct {
	mu            sync.Mutex
	lastConfigured *entity.IngressConfig
}

func (m *mockIngressManager) ConfigureRoute(ctx context.Context, route *entity.IngressConfig) error {
	m.mu.Lock()
	m.lastConfigured = route
	m.mu.Unlock()
	return nil
}

func (m *mockIngressManager) RemoveRoute(ctx context.Context, domain string) error {
	return nil
}

func (m *mockIngressManager) Reload(ctx context.Context) error {
	return nil
}

func TestDeployUseCase_ScaleOut(t *testing.T) {
	mockStrat := &mockStrategy{proto: entity.TargetTypeSSH}
	mockIngress := &mockIngressManager{}

	uc := usecase.NewDeployUseCase(nil, nil).
		WithStrategyResolver(&mockStrategyResolver{strat: mockStrat}).
		WithIngressManager(mockIngress)

	project := &entity.Project{
		Name: "scaled-web",
		Targets: []string{
			"ssh://deploy@192.168.1.101:22",
			"ssh://deploy@192.168.1.102:22",
			"ssh://deploy@192.168.1.103:22",
		},
		Services: map[string]*entity.Service{
			"web": {
				Name:  "web",
				Type:  entity.TypeDocker,
				Image: "bun-app:latest",
				Ports: []string{"3000:3000"},
				Ingress: &entity.IngressConfig{
					Provider:     "caddy",
					Domain:       "app.prod.com",
					UpstreamPort: 3000,
					LBPolicy:     "round_robin",
				},
			},
		},
	}

	var logBuf bytes.Buffer
	err := uc.Execute(context.Background(), project, "web", &logBuf)
	if err != nil {
		t.Fatalf("unexpected deploy error: %v", err)
	}

	// 1. Verify all 3 targets received deployments
	mockStrat.mu.Lock()
	count := len(mockStrat.deployedHosts)
	mockStrat.mu.Unlock()
	if count != 3 {
		t.Fatalf("expected 3 deployed hosts, got %d", count)
	}

	// 2. Verify Caddy Ingress was configured with 3 upstreams and round_robin policy
	mockIngress.mu.Lock()
	ing := mockIngress.lastConfigured
	mockIngress.mu.Unlock()

	if ing == nil {
		t.Fatalf("expected ingress to be configured")
	}
	if len(ing.Upstreams) != 3 {
		t.Fatalf("expected 3 upstreams in load balancer, got %d: %v", len(ing.Upstreams), ing.Upstreams)
	}
	if ing.LBPolicy != "round_robin" {
		t.Errorf("expected round_robin lb_policy, got %s", ing.LBPolicy)
	}
	if ing.Domain != "app.prod.com" {
		t.Errorf("expected domain app.prod.com, got %s", ing.Domain)
	}
}

func TestDeployUseCase_SingleTarget(t *testing.T) {
	mockStrat := &mockStrategy{proto: entity.TargetTypeSSH}
	mockIngress := &mockIngressManager{}

	uc := usecase.NewDeployUseCase(nil, nil).
		WithStrategyResolver(&mockStrategyResolver{strat: mockStrat}).
		WithIngressManager(mockIngress)

	project := &entity.Project{
		Name:    "single-web",
		Targets: []string{"ssh://deploy@10.0.0.1:22"},
		Services: map[string]*entity.Service{
			"api": {
				Name:  "api",
				Type:  entity.TypeDocker,
				Image: "api:v1",
				Ingress: &entity.IngressConfig{
					Provider:     "caddy",
					Domain:       "api.single.com",
					UpstreamPort: 8080,
				},
			},
		},
	}

	var logBuf bytes.Buffer
	err := uc.Execute(context.Background(), project, "api", &logBuf)
	if err != nil {
		t.Fatalf("unexpected deploy error: %v", err)
	}

	mockStrat.mu.Lock()
	count := len(mockStrat.deployedHosts)
	mockStrat.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 deployed host, got %d", count)
	}

	mockIngress.mu.Lock()
	ing := mockIngress.lastConfigured
	mockIngress.mu.Unlock()

	if ing == nil {
		t.Fatalf("expected ingress configuration")
	}
	if len(ing.Upstreams) != 1 || ing.Upstreams[0] != "10.0.0.1:8080" {
		t.Errorf("expected upstream '10.0.0.1:8080', got: %v", ing.Upstreams)
	}
}

func TestScaleUseCase(t *testing.T) {
	mockStrat := &mockStrategy{proto: entity.TargetTypeLocal}
	mockIngress := &mockIngressManager{}

	deployUC := usecase.NewDeployUseCase(nil, nil).
		WithStrategyResolver(&mockStrategyResolver{strat: mockStrat}).
		WithIngressManager(mockIngress)

	scaleUC := usecase.NewScaleUseCase(deployUC)

	project := &entity.Project{
		Name:    "scaled-app",
		Targets: []string{"localhost"},
		Services: map[string]*entity.Service{
			"web": {
				Name:  "web",
				Type:  entity.TypeDocker,
				Image: "bun-app:1.0",
				Ports: []string{"3000:3000"},
				Ingress: &entity.IngressConfig{
					Provider:     "caddy",
					Domain:       "scale.local",
					UpstreamPort: 3000,
				},
			},
		},
	}

	var logBuf bytes.Buffer
	err := scaleUC.Execute(context.Background(), project, "web", 3, &logBuf)
	if err != nil {
		t.Fatalf("unexpected scale error: %v", err)
	}

	if project.Services["web"].Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", project.Services["web"].Replicas)
	}
}

func TestRollbackUseCase(t *testing.T) {
	tmpDir := t.TempDir()
	mockStrat := &mockStrategy{proto: entity.TargetTypeLocal}
	mockIngress := &mockIngressManager{}

	deployUC := usecase.NewDeployUseCase(nil, nil).
		WithStrategyResolver(&mockStrategyResolver{strat: mockStrat}).
		WithIngressManager(mockIngress)

	rollbackUC := usecase.NewRollbackUseCase(deployUC, tmpDir)

	// Record two revisions
	rec1 := &entity.ReleaseRecord{
		Revision:    "rev-1",
		ServiceName: "web",
		Image:       "app:v1.0",
		Ports:       []string{"3000:3000"},
		Replicas:    1,
		CreatedAt:   time.Now().Add(-10 * time.Minute),
	}
	_ = usecase.RecordRelease(tmpDir, rec1)

	rec2 := &entity.ReleaseRecord{
		Revision:    "rev-2",
		ServiceName: "web",
		Image:       "app:v2.0-broken",
		Ports:       []string{"3000:3000"},
		Replicas:    1,
		CreatedAt:   time.Now(),
	}
	_ = usecase.RecordRelease(tmpDir, rec2)

	project := &entity.Project{
		Name:    "rollback-app",
		Targets: []string{"localhost"},
		Services: map[string]*entity.Service{
			"web": {
				Name:  "web",
				Type:  entity.TypeDocker,
				Image: "app:v2.0-broken",
			},
		},
	}

	var logBuf bytes.Buffer
	rolledBack, err := rollbackUC.Execute(context.Background(), project, "web", &logBuf)
	if err != nil {
		t.Fatalf("unexpected rollback error: %v", err)
	}

	if rolledBack.Revision != "rev-1" {
		t.Errorf("expected rollback to rev-1, got %s", rolledBack.Revision)
	}
	if project.Services["web"].Image != "app:v1.0" {
		t.Errorf("expected restored image app:v1.0, got %s", project.Services["web"].Image)
	}
}
