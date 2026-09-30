package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

type meshKeyData struct {
	NodeKey      string `json:"node_key"`
	PresharedKey string `json:"preshared_key"`
}

func loadOrCreateMeshKeys() (key.NodePrivate, tailcat.PresharedKey) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	keyDir := filepath.Join(home, ".kizuna")
	_ = os.MkdirAll(keyDir, 0700)
	keyFile := filepath.Join(keyDir, "mesh_key.json")

	if data, err := os.ReadFile(keyFile); err == nil {
		var kd meshKeyData
		if err := json.Unmarshal(data, &kd); err == nil && kd.NodeKey != "" {
			var nodeKey key.NodePrivate
			var psk tailcat.PresharedKey
			if err := nodeKey.UnmarshalText([]byte(kd.NodeKey)); err == nil {
				if kd.PresharedKey != "" {
					_ = psk.UnmarshalText([]byte(kd.PresharedKey))
				}
				return nodeKey, psk
			}
		}
	}

	// Generate persistent keys
	nodeKey := key.NewNode()
	psk := tailcat.NewPresharedKey()

	nodeKeyBytes, _ := nodeKey.MarshalText()
	pskBytes, _ := psk.MarshalText()

	kd := meshKeyData{
		NodeKey:      string(nodeKeyBytes),
		PresharedKey: string(pskBytes),
	}
	if b, err := json.MarshalIndent(kd, "", "  "); err == nil {
		_ = os.WriteFile(keyFile, b, 0600)
	}

	return nodeKey, psk
}

// TailcatMesh implements domain.MeshGateway using tailscale/tailcat
type TailcatMesh struct {
	mu              sync.Mutex
	nodeKey         key.NodePrivate
	psk             tailcat.PresharedKey
	keysLoaded      bool
	server          *tailcat.Server
	localLn         net.Listener
	isClosed        bool
	activeAddr      string
	derpRelay       *DERPRelay
	derpConfig      *entity.DERPConfig
	discoveredDERPs map[int]*entity.DERPNodeInfo
	activeDERP      *entity.DERPNodeInfo
	isBenchmarking  bool
	clients         map[string]*tailcat.Client
	sshPolicy       *entity.SSHConfig
	sshPeerLookup   func(pubKey string) *entity.Node
}

// NewMeshGateway creates a new instance of TailcatMesh
func NewMeshGateway() domain.MeshGateway {
	return &TailcatMesh{
		discoveredDERPs: make(map[int]*entity.DERPNodeInfo),
		clients:         make(map[string]*tailcat.Client),
	}
}

func (m *TailcatMesh) getOrLoadKeys() (key.NodePrivate, tailcat.PresharedKey) {
	if m.keysLoaded {
		return m.nodeKey, m.psk
	}
	m.nodeKey, m.psk = loadOrCreateMeshKeys()
	m.keysLoaded = true
	return m.nodeKey, m.psk
}

// SetDERPConfig configures and starts a private DERP relay on this node
func (m *TailcatMesh) SetDERPConfig(cfg *entity.DERPConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.derpConfig = cfg
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	nodeKey, _ := m.getOrLoadKeys()
	relay, err := NewDERPRelay(nodeKey, *cfg)
	if err != nil {
		return err
	}
	if err := relay.Start(); err != nil {
		return err
	}

	m.derpRelay = relay
	info := relay.NodeInfo()
	m.activeDERP = info
	m.discoveredDERPs[info.RegionID] = info
	return nil
}

// AddDiscoveredDERP registers a DERP relay discovered from peer nodes via Gossip
func (m *TailcatMesh) AddDiscoveredDERP(info *entity.DERPNodeInfo) {
	if info == nil || info.RegionID == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if this relay is already known and completely identical
	existing, exists := m.discoveredDERPs[info.RegionID]
	if exists && existing != nil &&
		existing.HostName == info.HostName &&
		existing.Port == info.Port &&
		existing.STUNPort == info.STUNPort &&
		existing.CertName == info.CertName {
		return
	}

	m.discoveredDERPs[info.RegionID] = info

	// If this node does not host its own DERP relay, benchmark and select the fastest one
	if m.derpRelay == nil && !m.isBenchmarking {
		m.isBenchmarking = true
		var candidates []*entity.DERPNodeInfo
		for _, d := range m.discoveredDERPs {
			candidates = append(candidates, d)
		}
		go func(cList []*entity.DERPNodeInfo) {
			defer func() {
				m.mu.Lock()
				m.isBenchmarking = false
				m.mu.Unlock()
			}()
			best := PickBestDERP(context.Background(), cList, 1500*time.Millisecond)
			if best != nil {
				m.mu.Lock()
				m.activeDERP = best
				m.mu.Unlock()
			}
		}(candidates)
	}
}

// GetActiveDERP returns the active DERP relay info (local or fastest remote)
func (m *TailcatMesh) GetActiveDERP() *entity.DERPNodeInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeDERP
}

// GetDiscoveredDERPs returns all discovered DERP relays across the mesh
func (m *TailcatMesh) GetDiscoveredDERPs() []*entity.DERPNodeInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	var list []*entity.DERPNodeInfo
	for _, d := range m.discoveredDERPs {
		list = append(list, d)
	}
	return list
}

// SetSSHPolicy configures inbound SSH access control and peer resolver
func (m *TailcatMesh) SetSSHPolicy(policy *entity.SSHConfig, peerLookup func(pubKey string) *entity.Node) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sshPolicy = policy
	m.sshPeerLookup = peerLookup
}

// ExtractNodePublicKey extracts the WireGuard public key from a node's Tailcat address
func ExtractNodePublicKey(addr string) string {
	if ci, err := tailcat.ParseAddr(tailcat.Addr(addr)); err == nil {
		return ci.ServerPublic.String()
	}
	return ""
}

// CheckSSHPermission evaluates whether an incoming SSH connection is permitted
// based on the configured SSH policy and caller node identity/tags.
func CheckSSHPermission(policy *entity.SSHConfig, caller *entity.Node, peerKey string) error {
	if policy == nil {
		return nil
	}
	if !policy.IsEnabled() {
		return fmt.Errorf("inbound SSH is disabled on this node")
	}

	// If no access control rules (allow/deny tags/nodes) are configured, permit by default
	if len(policy.AllowTags) == 0 && len(policy.DenyTags) == 0 &&
		len(policy.AllowNodes) == 0 && len(policy.DenyNodes) == 0 {
		return nil
	}

	// If ACL rules are defined but caller node could not be identified
	if caller == nil {
		cleanKey := strings.TrimPrefix(peerKey, "nodekey:")
		if len(cleanKey) > 16 {
			cleanKey = cleanKey[:16] + "..."
		}
		if cleanKey == "" {
			cleanKey = "unknown"
		}
		return fmt.Errorf("peer identity '%s' not recognized in paired cluster", cleanKey)
	}

	// 1. Check DenyNodes
	for _, denied := range policy.DenyNodes {
		d := strings.TrimSpace(denied)
		if d != "" && (strings.EqualFold(caller.Name, d) || strings.EqualFold(caller.ID, d)) {
			return fmt.Errorf("node '%s' is explicitly denied by deny_nodes policy", caller.Name)
		}
	}

	// 2. Check DenyTags
	for _, denyTag := range policy.DenyTags {
		dt := strings.TrimSpace(denyTag)
		if dt == "" {
			continue
		}
		for _, tag := range caller.Tags {
			if strings.EqualFold(strings.TrimSpace(tag), dt) {
				return fmt.Errorf("node '%s' has tag '%s' which is denied by deny_tags policy", caller.Name, tag)
			}
		}
	}

	// 3. Check AllowNodes and AllowTags
	hasAllowNodes := len(policy.AllowNodes) > 0
	hasAllowTags := len(policy.AllowTags) > 0

	// If neither allow list is configured, passing deny checks is sufficient
	if !hasAllowNodes && !hasAllowTags {
		return nil
	}

	nodeAllowed := false
	if hasAllowNodes {
		for _, allowed := range policy.AllowNodes {
			a := strings.TrimSpace(allowed)
			if a != "" && (strings.EqualFold(caller.Name, a) || strings.EqualFold(caller.ID, a)) {
				nodeAllowed = true
				break
			}
		}
	}

	tagAllowed := false
	if hasAllowTags {
		for _, allowTag := range policy.AllowTags {
			at := strings.TrimSpace(allowTag)
			if at == "" {
				continue
			}
			for _, tag := range caller.Tags {
				if strings.EqualFold(strings.TrimSpace(tag), at) {
					tagAllowed = true
					break
				}
			}
			if tagAllowed {
				break
			}
		}
	}

	if nodeAllowed || tagAllowed {
		return nil
	}

	if hasAllowTags && !hasAllowNodes {
		return fmt.Errorf("node '%s' (tags: %v) does not have any allowed tags (%v)", caller.Name, caller.Tags, policy.AllowTags)
	}
	if hasAllowNodes && !hasAllowTags {
		return fmt.Errorf("node '%s' is not in allow_nodes list (%v)", caller.Name, policy.AllowNodes)
	}
	return fmt.Errorf("node '%s' does not match allow_tags (%v) or allow_nodes (%v)", caller.Name, policy.AllowTags, policy.AllowNodes)
}

func (m *TailcatMesh) checkSSHAccess(s *tailcat.Server, c net.Conn) error {
	m.mu.Lock()
	policy := m.sshPolicy
	peerLookup := m.sshPeerLookup
	m.mu.Unlock()

	if policy == nil {
		return nil
	}
	if !policy.IsEnabled() {
		return fmt.Errorf("inbound SSH is disabled on this node")
	}

	var peerKey string
	if s != nil {
		for _, env := range s.PeerEnv(c.LocalAddr(), c.RemoteAddr()) {
			if strings.HasPrefix(env, "TAILCAT_PEER_KEY=") {
				peerKey = strings.TrimPrefix(env, "TAILCAT_PEER_KEY=")
				break
			}
		}
	}

	var caller *entity.Node
	if peerLookup != nil && peerKey != "" {
		caller = peerLookup(peerKey)
	}

	return CheckSSHPermission(policy, caller, peerKey)
}

// Listen starts a tailcat server or local fallback on the specified port
func (m *TailcatMesh) Listen(ctx context.Context, port uint16, handler func(net.Conn)) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Initialize tailcat.Server with persistent identity keys
	nodeKey, psk := m.getOrLoadKeys()

	var logf func(string, ...any)
	if os.Getenv("KIZUNA_DEBUG") == "" {
		logf = func(string, ...any) {}
	}

	var s *tailcat.Server
	s = &tailcat.Server{
		Key:          nodeKey,
		PresharedKey: psk,
		Logf:         logf,
		OnTCP: func(p uint16) func(net.Conn) {
			if p == port {
				return handler
			}
			if p == 22 {
				fallbackSSH := s.SSHConnHandler(tailcat.SSHOptions{
					Shell: true,
				})
				return func(c net.Conn) {
					// Enforce SSH tag-based access control policy
					if err := m.checkSSHAccess(s, c); err != nil {
						msg := fmt.Sprintf("Access denied by Kizuna SSH policy: %v\r\n", err)
						_, _ = c.Write([]byte(msg))
						time.Sleep(50 * time.Millisecond)
						_ = c.Close()
						return
					}

					// 1. Check if local OpenSSH daemon is reachable dynamically
					target, err := net.DialTimeout("tcp", "127.0.0.1:22", 200*time.Millisecond)
					if err == nil {
						tailcat.ProxyConns(target, c)
						return
					}
					// 2. Fallback to embedded tailcat SSH server (zero external dependency)
					fallbackSSH(c)
				}
			}
			return nil
		},
	}

	// If active DERP is configured or discovered, use it as the private bootstrap relay
	if m.activeDERP != nil {
		s.Region = BuildDERPRegion(m.activeDERP)
	}

	// Always bind local TCP listener for local CLI IPC (127.0.0.1) and direct LAN connectivity
	ln, localErr := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if localErr == nil {
		m.localLn = ln
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go handler(conn)
			}
		}()
	}

	if err := s.Start(); err != nil {
		if localErr != nil {
			return "", fmt.Errorf("failed to start tailcat (%v) and local fallback (%v)", err, localErr)
		}
		m.activeAddr = ln.Addr().String()
		return m.activeAddr, nil
	}

	m.server = s
	m.activeAddr = string(s.TailcatAddr())
	return m.activeAddr, nil
}

// Dial connects to a remote mesh node address or local fallback
func (m *TailcatMesh) Dial(ctx context.Context, addr string, port uint16) (net.Conn, error) {
	// If addr is a standard host:port, IP address, or local fallback, dial directly
	if strings.Contains(addr, ":") || net.ParseIP(addr) != nil || strings.HasPrefix(addr, "127.") || strings.HasPrefix(addr, "localhost") {
		var dialer net.Dialer
		if !strings.Contains(addr, ":") {
			addr = fmt.Sprintf("%s:%d", addr, port)
		}
		return dialer.DialContext(ctx, "tcp", addr)
	}

	// If addr cannot be parsed as a Tailcat address, treat as a hostname (e.g. Docker container, LAN host)
	ci, err := tailcat.ParseAddr(tailcat.Addr(addr))
	if err != nil {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", addr, port))
	}

	// If this address is the local node, loopback directly to local listener in <1ms
	m.mu.Lock()
	localNodeKey, _ := m.getOrLoadKeys()
	activeAddr := m.activeAddr
	activeDERP := m.activeDERP
	m.mu.Unlock()

	if ci.ServerPublic.NodePublic == localNodeKey.Public() || (activeAddr != "" && activeAddr == addr) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}

	// If we have an active private DERP relay (e.g. cloud relay on port 8443), prioritize it
	// so dials avoid blocked/slow third-party public relays (*.ipn.dev)
	if activeDERP != nil {
		privateRegion := BuildDERPRegion(activeDERP)
		isPrivateAlready := false
		for _, r := range ci.Region {
			if r != nil && r.RegionID == tailcfg.DERPRegionID(activeDERP.RegionID) {
				isPrivateAlready = true
				break
			}
		}
		if !isPrivateAlready {
			ci.Region = []*tailcfg.DERPRegion{privateRegion}
			ci.RegionID = 0
			addr = string(ci.Addr())
		}
	}

	// Dial via tailcat data-plane with persistent client key identity
	m.mu.Lock()
	if m.clients == nil {
		m.clients = make(map[string]*tailcat.Client)
	}
	cl, exists := m.clients[addr]
	if !exists {
		cl = tailcat.NewClient(tailcat.Addr(addr))
		cl.Key = localNodeKey
		if os.Getenv("KIZUNA_DEBUG") == "" {
			cl.Logf = func(string, ...any) {}
		}
		m.clients[addr] = cl
	}
	m.mu.Unlock()

	conn, err := cl.DialTCPPort(ctx, port)
	if err != nil {
		m.mu.Lock()
		delete(m.clients, addr)
		m.mu.Unlock()
		if closer, ok := any(cl).(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, err
	}
	return conn, nil
}

// Close gracefully closes the mesh server or listeners
func (m *TailcatMesh) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.isClosed {
		return nil
	}
	m.isClosed = true

	var err error
	for _, cl := range m.clients {
		if closer, ok := any(cl).(io.Closer); ok {
			_ = closer.Close()
		}
	}
	m.clients = make(map[string]*tailcat.Client)

	if m.derpRelay != nil {
		_ = m.derpRelay.Close()
	}
	if m.server != nil {
		err = m.server.Close()
	}
	if m.localLn != nil {
		_ = m.localLn.Close()
	}
	return err
}
