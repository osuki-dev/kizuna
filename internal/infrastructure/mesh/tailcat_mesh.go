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
	server          *tailcat.Server
	localLn         net.Listener
	isClosed        bool
	activeAddr      string
	derpRelay       *DERPRelay
	derpConfig      *entity.DERPConfig
	discoveredDERPs map[int]*entity.DERPNodeInfo
	activeDERP      *entity.DERPNodeInfo
}

// NewMeshGateway creates a new instance of TailcatMesh
func NewMeshGateway() domain.MeshGateway {
	return &TailcatMesh{
		discoveredDERPs: make(map[int]*entity.DERPNodeInfo),
	}
}

// SetDERPConfig configures and starts a private DERP relay on this node
func (m *TailcatMesh) SetDERPConfig(cfg *entity.DERPConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.derpConfig = cfg
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	nodeKey, _ := loadOrCreateMeshKeys()
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

	m.discoveredDERPs[info.RegionID] = info

	// If this node does not host its own DERP relay, benchmark and select the fastest one
	if m.derpRelay == nil {
		var candidates []*entity.DERPNodeInfo
		for _, d := range m.discoveredDERPs {
			candidates = append(candidates, d)
		}
		go func(cList []*entity.DERPNodeInfo) {
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

// Listen starts a tailcat server or local fallback on the specified port
func (m *TailcatMesh) Listen(ctx context.Context, port uint16, handler func(net.Conn)) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Initialize tailcat.Server with persistent identity keys
	nodeKey, psk := loadOrCreateMeshKeys()

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
					// 1. Check if local OpenSSH daemon is reachable dynamically
					target, err := net.DialTimeout("tcp", "127.0.0.1:22", 200*time.Millisecond)
					if err == nil {
						defer func() { _ = c.Close() }()
						defer func() { _ = target.Close() }()
						errCh := make(chan struct{}, 2)
						go func() {
							_, _ = io.Copy(target, c)
							if cw, ok := target.(interface{ CloseWrite() error }); ok {
								_ = cw.CloseWrite()
							}
							errCh <- struct{}{}
						}()
						go func() {
							_, _ = io.Copy(c, target)
							if cw, ok := c.(interface{ CloseWrite() error }); ok {
								_ = cw.CloseWrite()
							}
							errCh <- struct{}{}
						}()
						<-errCh
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
	localNodeKey, _ := loadOrCreateMeshKeys()
	if ci.ServerPublic.NodePublic == localNodeKey.Public() || (m.activeAddr != "" && m.activeAddr == addr) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}

	// If target address does not have custom DERP region embedded, but we have discovered private DERPs,
	// inject our active DERP region into the ConnInfo so the client dials our private DERP relay!
	if len(ci.Region) == 0 {
		m.mu.Lock()
		activeDERP := m.activeDERP
		m.mu.Unlock()
		if activeDERP != nil {
			ci.Region = []*tailcfg.DERPRegion{BuildDERPRegion(activeDERP)}
			addr = string(ci.Addr())
		}
	}

	// Dial via tailcat data-plane
	cl := tailcat.NewClient(tailcat.Addr(addr))
	if os.Getenv("KIZUNA_DEBUG") == "" {
		cl.Logf = func(string, ...any) {}
	}
	return cl.DialTCPPort(ctx, port)
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
