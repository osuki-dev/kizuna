package entity

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// ServiceType denotes the runtime workload type
type ServiceType string

const (
	TypeDocker  ServiceType = "docker"
	TypeCompose ServiceType = "compose"
	TypeBun     ServiceType = "bun"
	TypeNode    ServiceType = "node"
	TypeProcess ServiceType = "process"
)

// ServiceState represents runtime status
type ServiceState string

const (
	StateRunning   ServiceState = "running"
	StateStopped   ServiceState = "stopped"
	StateFailed    ServiceState = "failed"
	StateDeploying ServiceState = "deploying"
)

// Project represents the top-level workspace or monorepo
type Project struct {
	Version      string              `json:"version"`
	Name         string              `json:"name"`
	Target       string              `json:"target,omitempty"`
	Targets      []string            `json:"targets,omitempty"` // Multiple targets for horizontal scale-out
	Theme        string              `json:"theme,omitempty"`
	ActiveEnv    string              `json:"active_env,omitempty"`
	Environments []string            `json:"environments,omitempty"`
	Services     map[string]*Service `json:"services"`
	DERP         *DERPConfig         `json:"derp,omitempty" yaml:"derp,omitempty"`
}

// Service represents an individual deployable workload
type Service struct {
	Name        string            `json:"name"`
	Type        ServiceType       `json:"type"`
	Root        string            `json:"root"`
	Dockerfile  string            `json:"dockerfile,omitempty"`
	ComposeFile string            `json:"compose_file,omitempty"`
	Image       string            `json:"image,omitempty"`
	Ports       []string          `json:"ports,omitempty"`
	Replicas    int               `json:"replicas,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Build       *BuildConfig      `json:"build,omitempty"`
	Deploy      *DeployConfig     `json:"deploy,omitempty"`
	Ingress     *IngressConfig    `json:"ingress,omitempty"`
	Backup      *BackupConfig     `json:"backup,omitempty"`
	Logging     *LoggingConfig    `json:"logging,omitempty"`
	State       ServiceState      `json:"state"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// BuildConfig holds local/CI build instructions
type BuildConfig struct {
	Local    string `json:"local,omitempty" yaml:"local,omitempty"`
	Artifact string `json:"artifact,omitempty" yaml:"artifact,omitempty"`
}

// DeployConfig specifies commands and paths on destination node
type DeployConfig struct {
	Dest    string            `json:"dest,omitempty" yaml:"dest,omitempty"`
	Command string            `json:"command,omitempty" yaml:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

// IngressConfig configures Caddy reverse proxy and auto-HTTPS
type IngressConfig struct {
	Provider     string            `json:"provider" yaml:"provider"` // "caddy" or "none"
	Domain       string            `json:"domain,omitempty" yaml:"domain,omitempty"`
	AutoTLS      bool              `json:"auto_tls,omitempty" yaml:"auto_tls,omitempty"`
	TLS             string            `json:"tls,omitempty" yaml:"tls,omitempty"`                                       // "internal" (Local Root CA for LAN/IP), "cloudflare" (DNS-01 ACME), "auto", "none"
	DNSProvider     string            `json:"dns_provider,omitempty" yaml:"dns_provider,omitempty"`                     // DNS-01 provider (e.g. "cloudflare")
	CloudflareToken string            `json:"cloudflare_token,omitempty" yaml:"cloudflare_token,omitempty"`             // Cloudflare API Token for DNS-01 ACME
	DNSToken        string            `json:"dns_token,omitempty" yaml:"dns_token,omitempty"`                           // Generic DNS API Token
	UpstreamPort    int               `json:"upstream_port,omitempty" yaml:"upstream_port,omitempty"`
	Upstreams    []string          `json:"upstreams,omitempty" yaml:"upstreams,omitempty"`         // Multi-backend addresses for load balancing
	LBPolicy     string            `json:"lb_policy,omitempty" yaml:"lb_policy,omitempty"`         // "round_robin" (default), "least_conn", "random", "first"
	WebSocket    bool              `json:"websocket,omitempty" yaml:"websocket,omitempty"`         // One-click WebSocket proxy preset
	SSE          bool              `json:"sse,omitempty" yaml:"sse,omitempty"`                     // One-click SSE & AI streaming preset (flush_interval -1)
	GRPC         bool              `json:"grpc,omitempty" yaml:"grpc,omitempty"`                   // One-click gRPC h2c preset
	CORS         bool              `json:"cors,omitempty" yaml:"cors,omitempty"`                   // One-click CORS header preset
	Headers      *IngressHeaders   `json:"headers,omitempty" yaml:"headers,omitempty"`             // Custom request & response headers
	ProxyOptions *ProxyOptions     `json:"proxy_options,omitempty" yaml:"proxy_options,omitempty"` // SSE, buffer, TLS options
	Custom       []string          `json:"custom,omitempty" yaml:"custom,omitempty"`               // Raw Caddyfile directives (e.g. "encode gzip zstd", "basic_auth ...")
}

// IngressHeaders configures custom HTTP headers
type IngressHeaders struct {
	Request  map[string]string `json:"request,omitempty" yaml:"request,omitempty"`   // Injected via header_up
	Response map[string]string `json:"response,omitempty" yaml:"response,omitempty"` // Injected via header
}

// ProxyOptions defines reverse_proxy behavior tuning
type ProxyOptions struct {
	FlushInterval      string `json:"flush_interval,omitempty" yaml:"flush_interval,omitempty"` // "-1" for SSE/AI streaming
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify,omitempty"`
	MaxBufferSize      string `json:"max_buffer_size,omitempty" yaml:"max_buffer_size,omitempty"`
}

// BackupConfig specifies backup rules
type BackupConfig struct {
	Paths     []string              `json:"paths,omitempty" yaml:"paths,omitempty"`
	Database  *DatabaseBackupConfig `json:"database,omitempty" yaml:"database,omitempty"`
	Schedule  string                `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Retention *RetentionConfig      `json:"retention,omitempty" yaml:"retention,omitempty"`
	Storage   *StorageConfig        `json:"storage,omitempty" yaml:"storage,omitempty"`
}

// DatabaseBackupConfig defines how to backup local or remote databases
type DatabaseBackupConfig struct {
	Type      string `json:"type" yaml:"type"` // "postgres", "mysql", "sqlite", "custom"
	URI       string `json:"uri,omitempty" yaml:"uri,omitempty"` // Connection URI (remote or local)
	Path      string `json:"path,omitempty" yaml:"path,omitempty"` // For SQLite local file path
	Container string `json:"container,omitempty" yaml:"container,omitempty"` // Container name if running inside Docker
	Command   string `json:"command,omitempty" yaml:"command,omitempty"` // Custom backup command
}

// RetentionConfig defines how long backups are preserved before automatic cleanup
type RetentionConfig struct {
	KeepDays   int `json:"keep_days,omitempty" yaml:"keep_days,omitempty"`     // Keep backups for N days (0 to disable)
	MaxBackups int `json:"max_backups,omitempty" yaml:"max_backups,omitempty"` // Max number of backups to keep (0 to disable)
}

// LoggingConfig defines bounded log rotation to prevent filling server disk
type LoggingConfig struct {
	MaxSize string `json:"max_size,omitempty" yaml:"max_size,omitempty"` // e.g. "50m"
	MaxFile int    `json:"max_file,omitempty" yaml:"max_file,omitempty"` // e.g. 3
}

// ReleaseRecord represents a deployed revision for zero-downtime rollback
type ReleaseRecord struct {
	Revision    string    `json:"revision"`
	ServiceName string    `json:"service_name"`
	Image       string    `json:"image,omitempty"`
	Ports       []string  `json:"ports,omitempty"`
	Replicas    int       `json:"replicas,omitempty"`
	Upstreams   []string  `json:"upstreams,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// StorageConfig sets local or S3 backup destinations
type StorageConfig struct {
	Type      string `json:"type" yaml:"type"` // "local" or "s3"
	LocalDir  string `json:"local_dir,omitempty" yaml:"local_dir,omitempty"`
	Bucket    string `json:"bucket,omitempty" yaml:"bucket,omitempty"`
	Endpoint  string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"` // Custom endpoint for R2, B2, Wasabi, MinIO, etc.
	Region    string `json:"region,omitempty" yaml:"region,omitempty"`
	AccessKey string `json:"access_key,omitempty" yaml:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty" yaml:"secret_key,omitempty"`
	PathStyle *bool  `json:"path_style,omitempty" yaml:"path_style,omitempty"`
}

// EnvironmentConfig specifies environment-specific overrides (development, staging, production, etc.)
type EnvironmentConfig struct {
	Target   string                      `json:"target,omitempty" yaml:"target,omitempty"`
	Targets  []string                    `json:"targets,omitempty" yaml:"targets,omitempty"` // Environment-specific scale-out targets
	Theme    string                      `json:"theme,omitempty" yaml:"theme,omitempty"`
	Services map[string]*ServiceOverride `json:"services,omitempty" yaml:"services,omitempty"`

	// Shorthand overrides for single-service projects
	Ports    []string              `json:"ports,omitempty" yaml:"ports,omitempty"`
	Replicas int                   `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	Env      map[string]string     `json:"env,omitempty" yaml:"env,omitempty"`
	Build    *BuildConfig          `json:"build,omitempty" yaml:"build,omitempty"`
	Deploy   *DeployConfig         `json:"deploy,omitempty" yaml:"deploy,omitempty"`
	Ingress  *IngressConfig        `json:"ingress,omitempty" yaml:"ingress,omitempty"`
	Backup   *BackupConfig         `json:"backup,omitempty" yaml:"backup,omitempty"`
	Logging  *LoggingConfig        `json:"logging,omitempty" yaml:"logging,omitempty"`
}

// ServiceOverride holds per-service environment overrides
type ServiceOverride struct {
	Ports    []string              `json:"ports,omitempty" yaml:"ports,omitempty"`
	Replicas int                   `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	Env      map[string]string     `json:"env,omitempty" yaml:"env,omitempty"`
	Build    *BuildConfig          `json:"build,omitempty" yaml:"build,omitempty"`
	Deploy   *DeployConfig         `json:"deploy,omitempty" yaml:"deploy,omitempty"`
	Ingress  *IngressConfig        `json:"ingress,omitempty" yaml:"ingress,omitempty"`
	Backup   *BackupConfig         `json:"backup,omitempty" yaml:"backup,omitempty"`
	Logging  *LoggingConfig        `json:"logging,omitempty" yaml:"logging,omitempty"`
}

// Node represents a remote machine in the mesh
type Node struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Addr        string      `json:"addr"` // Tailcat mesh address
	AuthToken   string      `json:"auth_token,omitempty"`
	IsOnline    bool        `json:"is_online"`
	LastSeen    time.Time   `json:"last_seen"`
	OS          string      `json:"os"`
	Arch        string      `json:"arch"`
	Tags        []string    `json:"tags,omitempty" yaml:"tags,omitempty"`
	Host        string      `json:"host,omitempty" yaml:"host,omitempty"` // Hostname, domain, or IP (e.g. mac-mini.local or 10.0.0.9)
	IP          string      `json:"ip,omitempty" yaml:"ip,omitempty"`     // Legacy/convenience alias for Host
	Status      string      `json:"status,omitempty" yaml:"status,omitempty"` // "alive", "suspect", "dead", "offline"
	GossipState GossipState `json:"gossip_state,omitempty" yaml:"gossip_state,omitempty"` // Internal protocol state
	Incarnation uint64      `json:"incarnation,omitempty" yaml:"incarnation,omitempty"`
	CPUUsage    float64     `json:"cpu_usage"`
	MemoryUsage float64     `json:"memory_usage"`
	DiskUsage   float64     `json:"disk_usage"`
	TotalMemory uint64      `json:"total_memory,omitempty"`
	UsedMemory  uint64      `json:"used_memory,omitempty"`
	TotalDisk   uint64      `json:"total_disk,omitempty"`
	UsedDisk    uint64      `json:"used_disk,omitempty"`
	CPUCores    int         `json:"cpu_cores,omitempty"`
	Uptime      uint64      `json:"uptime,omitempty"`
	Load1       float64       `json:"load1,omitempty"`
	LatencyMs   int64         `json:"latency_ms,omitempty"`
	DERP        *DERPNodeInfo `json:"derp,omitempty" yaml:"derp,omitempty"`
}

// DERPConfig represents configuration for hosting a private DERP relay
type DERPConfig struct {
	Enabled    bool   `json:"enabled" yaml:"enabled"`
	Host       string `json:"host,omitempty" yaml:"host,omitempty"`             // Reachable hostname or IP (e.g. 10.0.0.4 or vps.example.com)
	Port       int    `json:"port,omitempty" yaml:"port,omitempty"`             // TLS/TCP port (default 8443)
	STUNPort   int    `json:"stun_port,omitempty" yaml:"stun_port,omitempty"`   // STUN UDP port (default 3478, or 0 if disabled)
	RegionID   int    `json:"region_id,omitempty" yaml:"region_id,omitempty"`   // Custom Region ID (range 900-999)
	RegionCode string `json:"region_code,omitempty" yaml:"region_code,omitempty"`
	RegionName string `json:"region_name,omitempty" yaml:"region_name,omitempty"`
}

// DERPNodeInfo represents advertised DERP relay capability of a node
type DERPNodeInfo struct {
	RegionID   int    `json:"region_id" yaml:"region_id"`
	RegionCode string `json:"region_code" yaml:"region_code"`
	RegionName string `json:"region_name" yaml:"region_name"`
	HostName   string `json:"host_name" yaml:"host_name"`
	Port       int    `json:"port" yaml:"port"`
	STUNPort   int    `json:"stun_port,omitempty" yaml:"stun_port,omitempty"`
	CertName   string `json:"cert_name,omitempty" yaml:"cert_name,omitempty"` // "sha256-raw:<hex>" for self-signed certs
	IPv4       string `json:"ipv4,omitempty" yaml:"ipv4,omitempty"`
	IPv6       string `json:"ipv6,omitempty" yaml:"ipv6,omitempty"`
}

// GossipState represents node membership lifecycle in the Gossip cluster
type GossipState string

const (
	GossipStateAlive   GossipState = "alive"
	GossipStateSuspect GossipState = "suspect"
	GossipStateDead    GossipState = "dead"
	GossipStateLeft    GossipState = "left"
)

// GossipMessageType represents the type of Gossip protocol message
type GossipMessageType string

const (
	GossipMsgPing         GossipMessageType = "ping"
	GossipMsgAck          GossipMessageType = "ack"
	GossipMsgIndirectPing GossipMessageType = "indirect_ping"
	GossipMsgSyncReq      GossipMessageType = "sync_req"
	GossipMsgSyncResp     GossipMessageType = "sync_resp"
	GossipMsgUpdate       GossipMessageType = "update"
)

// GossipMessage is the protocol envelope for peer-to-peer gossip exchanges
type GossipMessage struct {
	Type        GossipMessageType `json:"type"`
	SenderID    string            `json:"sender_id"`
	SenderName  string            `json:"sender_name"`
	SenderAddr  string            `json:"sender_addr"`
	SenderDERP  *DERPNodeInfo     `json:"sender_derp,omitempty"`
	TargetID    string            `json:"target_id,omitempty"` // Used for indirect ping
	Incarnation uint64            `json:"incarnation"`
	Updates     []*GossipUpdate   `json:"updates,omitempty"`
	Digest      []*GossipDigest   `json:"digest,omitempty"`
}

// GossipUpdate carries a membership or metadata state change event
type GossipUpdate struct {
	Node        *Node       `json:"node"`
	State       GossipState `json:"state"`
	Incarnation uint64      `json:"incarnation"`
	Timestamp   time.Time   `json:"timestamp"`
}

// GossipDigest summarizes node state for anti-entropy full synchronization
type GossipDigest struct {
	NodeID      string      `json:"node_id"`
	NodeName    string      `json:"node_name"`
	Incarnation uint64      `json:"incarnation"`
	State       GossipState `json:"state"`
}

// GossipEngineStatus represents diagnostic status of the local Gossip engine
type GossipEngineStatus struct {
	NodeID       string         `json:"node_id"`
	NodeName     string         `json:"node_name"`
	MeshAddr     string         `json:"mesh_addr"`
	State        GossipState    `json:"state"`
	Incarnation  uint64         `json:"incarnation"`
	Protocol     string         `json:"protocol"`
	TotalMembers int            `json:"total_members"`
	AliveCount   int            `json:"alive_count"`
	SuspectCount int            `json:"suspect_count"`
	DeadCount    int            `json:"dead_count"`
	IntervalMs   int64          `json:"interval_ms"`
	Members      []*Node        `json:"members,omitempty"`
}

// NodeMetaUpdate represents a payload to update node metadata
type NodeMetaUpdate struct {
	Tags []string `json:"tags,omitempty"`
	Host string   `json:"host,omitempty"`
	IP   string   `json:"ip,omitempty"`
}

// PairingPayload represents pairing credentials
type PairingPayload struct {
	ClientName string `json:"client_name"`
	PIN        string `json:"pin"`
}

// PairingResult represents the pairing result from the agent
type PairingResult struct {
	Success   bool   `json:"success"`
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name"`
	AuthToken string `json:"auth_token"`
	Error     string `json:"error,omitempty"`
}

// BackupRecord represents an executed backup archive
type BackupRecord struct {
	ID          string    `json:"id"`
	ServiceName string    `json:"service_name"`
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	StorageType string    `json:"storage_type"`
	CreatedAt   time.Time `json:"created_at"`
}

// TargetType defines the protocol/transport to reach a target host
type TargetType string

const (
	TargetTypeMesh  TargetType = "mesh"  // Kizuna mesh agent node via WireGuard/Tailcat
	TargetTypeSSH   TargetType = "ssh"   // Agentless remote server via SSH (zero kizuna required)
	TargetTypeLocal TargetType = "local" // Local machine
)

// TargetHost represents a resolved destination target
type TargetHost struct {
	Raw      string     `json:"raw"`
	Type     TargetType `json:"type"`
	Host     string     `json:"host"`
	Port     int        `json:"port"`
	User     string     `json:"user,omitempty"`
	KeyPath  string     `json:"key_path,omitempty"`
	Password string     `json:"password,omitempty"`
}

// Address returns host:port or host string
func (t *TargetHost) Address() string {
	if t.Port > 0 && t.Port != 22 {
		return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	}
	return t.Host
}

// DeployResult captures the execution outcome of deploying a service on a target
type DeployResult struct {
	Target      string        `json:"target"`
	ServiceName string        `json:"service_name"`
	Success     bool          `json:"success"`
	Duration    time.Duration `json:"duration"`
	ContainerID string        `json:"container_id,omitempty"`
	Error       string        `json:"error,omitempty"`
}

// ParseTargetHost parses target string (e.g. "ssh://user@host:22", "user@host", "192.168.1.100", "localhost", "node-name")
func ParseTargetHost(raw string) *TargetHost {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "localhost" || raw == "127.0.0.1" {
		return &TargetHost{
			Raw:  raw,
			Type: TargetTypeLocal,
			Host: "127.0.0.1",
			Port: 0,
		}
	}

	if strings.HasPrefix(raw, "ssh://") {
		trimmed := strings.TrimPrefix(raw, "ssh://")
		user := "root"
		hostPort := trimmed
		if idx := strings.Index(trimmed, "@"); idx != -1 {
			user = trimmed[:idx]
			hostPort = trimmed[idx+1:]
		}
		host := hostPort
		port := 22
		if h, pStr, err := net.SplitHostPort(hostPort); err == nil {
			host = h
			if p, err := strconv.Atoi(pStr); err == nil {
				port = p
			}
		}
		return &TargetHost{
			Raw:  raw,
			Type: TargetTypeSSH,
			Host: host,
			Port: port,
			User: user,
		}
	}

	if strings.Contains(raw, "@") {
		parts := strings.SplitN(raw, "@", 2)
		user := parts[0]
		hostPort := parts[1]
		host := hostPort
		port := 22
		if h, pStr, err := net.SplitHostPort(hostPort); err == nil {
			host = h
			if p, err := strconv.Atoi(pStr); err == nil {
				port = p
			}
		}
		return &TargetHost{
			Raw:  raw,
			Type: TargetTypeSSH,
			Host: host,
			Port: port,
			User: user,
		}
	}

	// If it's a numeric IP address
	if net.ParseIP(raw) != nil {
		return &TargetHost{
			Raw:  raw,
			Type: TargetTypeSSH,
			Host: raw,
			Port: 22,
			User: "root",
		}
	}

	// Default to Mesh node name
	return &TargetHost{
		Raw:  raw,
		Type: TargetTypeMesh,
		Host: raw,
	}
}
