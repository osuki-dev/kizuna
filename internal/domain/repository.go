package domain

import (
	"context"
	"io"
	"net"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// WorkloadRunner defines operations to execute and monitor services (Docker, Compose, Bun, etc.)
type WorkloadRunner interface {
	Deploy(ctx context.Context, svc *entity.Service, artifactReader io.Reader) error
	Stop(ctx context.Context, serviceName string) error
	GetStatus(ctx context.Context, serviceName string) (*entity.Service, error)
	ListServices(ctx context.Context) ([]*entity.Service, error)
	StreamLogs(ctx context.Context, serviceName string, writer io.Writer) error
}

// IngressManager defines operations for Caddy reverse proxy and auto-HTTPS
type IngressManager interface {
	ConfigureRoute(ctx context.Context, route *entity.IngressConfig) error
	RemoveRoute(ctx context.Context, domain string) error
	Reload(ctx context.Context) error
}

// BackupManager defines operations for backup snapshots
type BackupManager interface {
	CreateBackup(ctx context.Context, svcName string, cfg *entity.BackupConfig) (*entity.BackupRecord, error)
	ListBackups(ctx context.Context, svcName string) ([]*entity.BackupRecord, error)
}

// NodeRepository manages local storage of paired nodes on the CLI side
type NodeRepository interface {
	GetNode(name string) (*entity.Node, error)
	SaveNode(node *entity.Node) error
	ListNodes() ([]*entity.Node, error)
	DeleteNode(name string) error
}

// MeshGateway provides P2P mesh connectivity (Tailcat wrapper)
type MeshGateway interface {
	Listen(ctx context.Context, port uint16, handler func(net.Conn)) (string, error)
	Dial(ctx context.Context, addr string, port uint16) (net.Conn, error)
	Close() error
	SetDERPConfig(cfg *entity.DERPConfig) error
	AddDiscoveredDERP(info *entity.DERPNodeInfo)
	GetActiveDERP() *entity.DERPNodeInfo
	GetDiscoveredDERPs() []*entity.DERPNodeInfo
	SetSSHPolicy(policy *entity.SSHConfig, peerLookup func(pubKey string) *entity.Node)
}

// AuthManager handles agent-side pairing PIN verification and token validation
type AuthManager interface {
	GeneratePIN() string
	GetActivePIN() (string, bool)
	VerifyPIN(pin, clientName string) (string, error)
	ValidateToken(token string) bool
	ValidateTokenFromAddr(token, remoteAddr string) bool
	RevokeClient(nameOrID string) error
	RevokeToken(token string) error
}

// TargetDeploymentStrategy represents the Strategy Pattern for deploying workloads to different target environments
// (e.g. Mesh node via agent, Agentless remote server via SSH, or Local machine)
type TargetDeploymentStrategy interface {
	Protocol() entity.TargetType
	Deploy(ctx context.Context, target *entity.TargetHost, svc *entity.Service, artifact io.Reader) (*entity.DeployResult, error)
	Stop(ctx context.Context, target *entity.TargetHost, serviceName string) error
	GetStatus(ctx context.Context, target *entity.TargetHost, serviceName string) (*entity.Service, error)
}
