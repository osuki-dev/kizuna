package strategy_test

import (
	"context"
	"io"
	"testing"

	"github.com/osuki-dev/kizuna/internal/adapter/strategy"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

type mockStrategy struct {
	proto  entity.TargetType
	called bool
}

func (m *mockStrategy) Protocol() entity.TargetType {
	return m.proto
}

func (m *mockStrategy) Deploy(ctx context.Context, target *entity.TargetHost, svc *entity.Service, artifact io.Reader) (*entity.DeployResult, error) {
	m.called = true
	return &entity.DeployResult{
		Target:      target.Address(),
		ServiceName: svc.Name,
		Success:     true,
	}, nil
}

func (m *mockStrategy) Stop(ctx context.Context, target *entity.TargetHost, serviceName string) error {
	return nil
}

func (m *mockStrategy) GetStatus(ctx context.Context, target *entity.TargetHost, serviceName string) (*entity.Service, error) {
	return &entity.Service{Name: serviceName, State: entity.StateRunning}, nil
}

func TestParseTargetHost(t *testing.T) {
	tests := []struct {
		input        string
		expectedType entity.TargetType
		expectedHost string
		expectedPort int
		expectedUser string
	}{
		{
			input:        "localhost",
			expectedType: entity.TargetTypeLocal,
			expectedHost: "127.0.0.1",
			expectedPort: 0,
		},
		{
			input:        "ssh://deploy@192.168.1.100:2222",
			expectedType: entity.TargetTypeSSH,
			expectedHost: "192.168.1.100",
			expectedPort: 2222,
			expectedUser: "deploy",
		},
		{
			input:        "root@10.0.0.5",
			expectedType: entity.TargetTypeSSH,
			expectedHost: "10.0.0.5",
			expectedPort: 22,
			expectedUser: "root",
		},
		{
			input:        "172.16.0.10",
			expectedType: entity.TargetTypeSSH,
			expectedHost: "172.16.0.10",
			expectedPort: 22,
			expectedUser: "root",
		},
		{
			input:        "my-mesh-worker-node",
			expectedType: entity.TargetTypeMesh,
			expectedHost: "my-mesh-worker-node",
		},
	}

	for _, tc := range tests {
		th := entity.ParseTargetHost(tc.input)
		if th.Type != tc.expectedType {
			t.Errorf("input '%s': expected type %s, got %s", tc.input, tc.expectedType, th.Type)
		}
		if th.Host != tc.expectedHost {
			t.Errorf("input '%s': expected host %s, got %s", tc.input, tc.expectedHost, th.Host)
		}
		if tc.expectedPort > 0 && th.Port != tc.expectedPort {
			t.Errorf("input '%s': expected port %d, got %d", tc.input, tc.expectedPort, th.Port)
		}
		if tc.expectedUser != "" && th.User != tc.expectedUser {
			t.Errorf("input '%s': expected user %s, got %s", tc.input, tc.expectedUser, th.User)
		}
	}
}

func TestStrategyFactory(t *testing.T) {
	localMock := &mockStrategy{proto: entity.TargetTypeLocal}
	meshMock := &mockStrategy{proto: entity.TargetTypeMesh}
	sshMock := &mockStrategy{proto: entity.TargetTypeSSH}

	factory := strategy.NewFactory(localMock, meshMock, sshMock)

	// 1. Resolve Local
	strat, err := factory.GetStrategy(&entity.TargetHost{Type: entity.TargetTypeLocal})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strat.Protocol() != entity.TargetTypeLocal {
		t.Errorf("expected local protocol, got %s", strat.Protocol())
	}

	// 2. Resolve SSH Agentless
	strat, err = factory.GetStrategy(&entity.TargetHost{Type: entity.TargetTypeSSH})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strat.Protocol() != entity.TargetTypeSSH {
		t.Errorf("expected ssh protocol, got %s", strat.Protocol())
	}

	// 3. Resolve Mesh Agent
	strat, err = factory.GetStrategy(&entity.TargetHost{Type: entity.TargetTypeMesh})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strat.Protocol() != entity.TargetTypeMesh {
		t.Errorf("expected mesh protocol, got %s", strat.Protocol())
	}
}
