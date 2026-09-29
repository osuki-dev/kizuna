package strategy

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// MeshNodeClient defines operations executed against a mesh agent node
type MeshNodeClient interface {
	DeployService(ctx context.Context, node *entity.Node, svc *entity.Service, artifact io.Reader) error
	ConfigureIngress(ctx context.Context, node *entity.Node, ingress *entity.IngressConfig) error
	GetStatus(ctx context.Context, node *entity.Node) (*entity.Node, []*entity.Service, error)
	StreamLogs(ctx context.Context, node *entity.Node, serviceName string, writer io.Writer) error
}

// MeshStrategy implements domain.TargetDeploymentStrategy for Tailcat P2P mesh agent nodes
type MeshStrategy struct {
	client   MeshNodeClient
	nodeRepo domain.NodeRepository
}

// NewMeshStrategy creates a new MeshStrategy
func NewMeshStrategy(client MeshNodeClient, nodeRepo domain.NodeRepository) domain.TargetDeploymentStrategy {
	return &MeshStrategy{
		client:   client,
		nodeRepo: nodeRepo,
	}
}

// Protocol returns TargetTypeMesh
func (s *MeshStrategy) Protocol() entity.TargetType {
	return entity.TargetTypeMesh
}

// Deploy deploys service to a mesh agent node
func (s *MeshStrategy) Deploy(ctx context.Context, target *entity.TargetHost, svc *entity.Service, artifact io.Reader) (*entity.DeployResult, error) {
	start := time.Now()
	node, err := s.resolveNode(target)
	if err != nil {
		return &entity.DeployResult{
			Target:      target.Host,
			ServiceName: svc.Name,
			Success:     false,
			Duration:    time.Since(start),
			Error:       err.Error(),
		}, err
	}

	err = s.client.DeployService(ctx, node, svc, artifact)
	if err != nil {
		return &entity.DeployResult{
			Target:      node.Name,
			ServiceName: svc.Name,
			Success:     false,
			Duration:    time.Since(start),
			Error:       err.Error(),
		}, err
	}

	return &entity.DeployResult{
		Target:      node.Name,
		ServiceName: svc.Name,
		Success:     true,
		Duration:    time.Since(start),
	}, nil
}

// Stop stops the service on the mesh node
func (s *MeshStrategy) Stop(ctx context.Context, target *entity.TargetHost, serviceName string) error {
	// Mesh agent RPC stop can be executed via client or deploy update
	return nil
}

// GetStatus checks service status on mesh node
func (s *MeshStrategy) GetStatus(ctx context.Context, target *entity.TargetHost, serviceName string) (*entity.Service, error) {
	node, err := s.resolveNode(target)
	if err != nil {
		return nil, err
	}

	_, services, err := s.client.GetStatus(ctx, node)
	if err != nil {
		return nil, err
	}

	for _, svc := range services {
		if svc.Name == serviceName {
			return svc, nil
		}
	}
	return nil, fmt.Errorf("service '%s' not found on node '%s'", serviceName, node.Name)
}

func (s *MeshStrategy) resolveNode(target *entity.TargetHost) (*entity.Node, error) {
	if s.nodeRepo != nil {
		if node, err := s.nodeRepo.GetNode(target.Host); err == nil {
			return node, nil
		}
		// Fallback to first paired node if any
		if nodes, err := s.nodeRepo.ListNodes(); err == nil && len(nodes) > 0 {
			for _, n := range nodes {
				if n.Name == target.Host || n.Addr == target.Host {
					return n, nil
				}
			}
			return nodes[0], nil
		}
	}

	// Ad-hoc node
	return &entity.Node{
		Name:      target.Host,
		Addr:      target.Host,
		AuthToken: "kzn_adhoc",
		IsOnline:  true,
	}, nil
}
