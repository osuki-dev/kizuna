package mesh

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/tailscale/tailcat"
)

// TailcatMesh implements domain.MeshGateway using tailscale/tailcat
type TailcatMesh struct {
	mu           sync.Mutex
	server       *tailcat.Server
	localLn      net.Listener
	isClosed     bool
	activeAddr   string
}

// NewMeshGateway creates a new instance of TailcatMesh
func NewMeshGateway() domain.MeshGateway {
	return &TailcatMesh{}
}

// Listen starts a tailcat server or local fallback on the specified port
func (m *TailcatMesh) Listen(ctx context.Context, port uint16, handler func(net.Conn)) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Initialize tailcat.Server
	var logf func(string, ...any)
	if os.Getenv("KIZUNA_DEBUG") == "" {
		logf = func(string, ...any) {}
	}

	s := &tailcat.Server{
		Logf: logf,
		OnTCP: func(p uint16) func(net.Conn) {
			if p == port {
				return handler
			}
			return nil
		},
	}

	if err := s.Start(); err != nil {
		// Fallback to TCP listener on all interfaces if tailcat fails to start (e.g. offline/isolated environment)
		ln, localErr := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if localErr != nil {
			return "", fmt.Errorf("failed to start tailcat (%v) and local fallback (%v)", err, localErr)
		}
		m.localLn = ln
		m.activeAddr = ln.Addr().String()

		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go handler(conn)
			}
		}()

		return m.activeAddr, nil
	}

	m.server = s
	m.activeAddr = string(s.TailcatAddr())
	return m.activeAddr, nil
}

// Dial connects to a remote mesh node address or local fallback
func (m *TailcatMesh) Dial(ctx context.Context, addr string, port uint16) (net.Conn, error) {
	// If addr is a standard host:port or IP address, dial directly
	if strings.Contains(addr, ":") || strings.HasPrefix(addr, "127.0.0.1") || strings.HasPrefix(addr, "localhost") {
		var dialer net.Dialer
		if !strings.Contains(addr, ":") {
			addr = fmt.Sprintf("%s:%d", addr, port)
		}
		return dialer.DialContext(ctx, "tcp", addr)
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
	if m.server != nil {
		err = m.server.Close()
	}
	if m.localLn != nil {
		_ = m.localLn.Close()
	}
	return err
}
