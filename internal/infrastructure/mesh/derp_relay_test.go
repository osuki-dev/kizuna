package mesh

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

func TestLoadOrCreateDERPCert(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "kizuna-derp-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	origHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", origHome) }()
	_ = os.Setenv("HOME", tempDir)

	cert1, fp1, err := loadOrCreateDERPCert("127.0.0.1")
	if err != nil {
		t.Fatalf("first loadOrCreateDERPCert failed: %v", err)
	}
	if fp1 == "" || len(cert1.Certificate) == 0 {
		t.Fatalf("expected non-empty cert and fingerprint, got fp=%s", fp1)
	}
	if len(fp1) < 15 || fp1[:11] != "sha256-raw:" {
		t.Fatalf("fingerprint should start with sha256-raw:, got: %s", fp1)
	}

	certFile := filepath.Join(tempDir, ".kizuna", "derp_cert.json")
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		t.Fatalf("expected derp_cert.json to exist at %s", certFile)
	}

	// Loading again should reuse the existing cert and have the exact same fingerprint
	cert2, fp2, err := loadOrCreateDERPCert("127.0.0.1")
	if err != nil {
		t.Fatalf("second loadOrCreateDERPCert failed: %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("expected cached fingerprint %s, got %s", fp1, fp2)
	}
	if len(cert2.Certificate) == 0 {
		t.Fatalf("expected cached cert to be loaded")
	}
}

func TestDERPRelayLifecycle(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "kizuna-relay-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	origHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", origHome) }()
	_ = os.Setenv("HOME", tempDir)

	nodeKey := key.NewNode()
	cfg := entity.DERPConfig{
		Enabled:    true,
		Host:       "127.0.0.1",
		Port:       28443,
		STUNPort:   23478,
		RegionID:   950,
		RegionCode: "test-relay",
		RegionName: "Test Private DERP",
	}

	relay, err := NewDERPRelay(nodeKey, cfg)
	if err != nil {
		t.Fatalf("NewDERPRelay failed: %v", err)
	}

	if err := relay.Start(); err != nil {
		t.Fatalf("relay.Start failed: %v", err)
	}
	defer func() { _ = relay.Close() }()

	info := relay.NodeInfo()
	if info.RegionID != 950 || info.HostName != "127.0.0.1" || info.Port != 28443 || info.STUNPort != 23478 {
		t.Fatalf("unexpected NodeInfo: %+v", info)
	}
	if info.CertName == "" {
		t.Fatalf("expected non-empty CertName fingerprint")
	}

	// Probe STUN
	rtt, err := probeSTUN("127.0.0.1", 23478, 2*time.Second)
	if err != nil {
		t.Fatalf("probeSTUN failed: %v", err)
	}
	if rtt <= 0 {
		t.Fatalf("expected positive RTT, got %v", rtt)
	}

	// Test MeasureDERPLatency
	measRTT, err := MeasureDERPLatency(info, 2*time.Second)
	if err != nil {
		t.Fatalf("MeasureDERPLatency failed: %v", err)
	}
	if measRTT <= 0 || measRTT >= 5*time.Second {
		t.Fatalf("expected reasonable latency measurement, got %v", measRTT)
	}

	// Close relay
	if err := relay.Close(); err != nil {
		t.Fatalf("relay.Close failed: %v", err)
	}
}

func TestPickBestDERP(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "kizuna-pick-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	origHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", origHome) }()
	_ = os.Setenv("HOME", tempDir)

	nodeKey := key.NewNode()
	cfg := entity.DERPConfig{
		Enabled:    true,
		Host:       "127.0.0.1",
		Port:       28444,
		STUNPort:   23479,
		RegionID:   951,
		RegionCode: "fast-relay",
		RegionName: "Fast Local Relay",
	}

	relay, err := NewDERPRelay(nodeKey, cfg)
	if err != nil {
		t.Fatalf("NewDERPRelay failed: %v", err)
	}
	if err := relay.Start(); err != nil {
		t.Fatalf("relay.Start failed: %v", err)
	}
	defer func() { _ = relay.Close() }()

	fastInfo := relay.NodeInfo()

	// Slow / unreachable mock candidate
	slowInfo := &entity.DERPNodeInfo{
		RegionID:   952,
		RegionCode: "unreachable-relay",
		HostName:   "192.0.2.1", // TEST-NET-1 unroutable
		Port:       8443,
		STUNPort:   3478,
	}

	best := PickBestDERP(context.Background(), []*entity.DERPNodeInfo{slowInfo, fastInfo}, 500*time.Millisecond)
	if best == nil {
		t.Fatalf("expected PickBestDERP to return a node, got nil")
	}
	if best.RegionID != 951 {
		t.Fatalf("expected fast relay 951 to be selected, got %d (%s)", best.RegionID, best.RegionName)
	}
}

func TestBuildDERPRegionAndTailcatIntegration(t *testing.T) {
	info := &entity.DERPNodeInfo{
		RegionID:   901,
		RegionCode: "kzn",
		RegionName: "Private Relay",
		HostName:   "10.0.0.4",
		Port:       8443,
		STUNPort:   3478,
		CertName:   "sha256-raw:abcdef123456",
		IPv4:       "10.0.0.4",
	}

	reg := BuildDERPRegion(info)
	if reg == nil {
		t.Fatalf("BuildDERPRegion returned nil")
	}
	if reg.RegionID != 901 || reg.RegionCode != "kzn" {
		t.Fatalf("unexpected region: %+v", reg)
	}
	if len(reg.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(reg.Nodes))
	}
	n := reg.Nodes[0]
	if n.HostName != "10.0.0.4" || n.DERPPort != 8443 || n.STUNPort != 3478 || n.CertName != "sha256-raw:abcdef123456" {
		t.Fatalf("unexpected node in region: %+v", n)
	}

	// Verify that tailcat ConnInfo can roundtrip this region in an Addr
	sKey := key.NewNode()
	ci := tailcat.ConnInfo{
		ServerPublic: tailcat.NodePublic{NodePublic: sKey.Public()},
		Region:       []*tailcfg.DERPRegion{reg},
	}
	addr := ci.Addr()
	if addr == "" {
		t.Fatalf("Addr() returned empty string")
	}

	parsed, err := tailcat.ParseAddr(addr)
	if err != nil {
		t.Fatalf("ParseAddr failed: %v", err)
	}
	if len(parsed.Region) != 1 {
		t.Fatalf("expected 1 region in parsed addr, got %d", len(parsed.Region))
	}
	parsedReg := parsed.Region[0]
	if len(parsedReg.Nodes) != 1 {
		t.Fatalf("expected 1 node in parsed region, got %d", len(parsedReg.Nodes))
	}
	node := parsedReg.Nodes[0]
	if node.HostName != "10.0.0.4" || node.DERPPort != 8443 || node.STUNPort != 3478 || node.CertName != "sha256-raw:abcdef123456" {
		t.Fatalf("mismatched parsed node: %+v", node)
	}
}

func TestPrivateDERPEndToEnd(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "kizuna-e2e-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	origHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", origHome) }()
	_ = os.Setenv("HOME", tempDir)

	// 1. Start private DERP relay
	derpNodeKey := key.NewNode()
	cfg := entity.DERPConfig{
		Enabled:    true,
		Host:       "127.0.0.1",
		Port:       28445,
		STUNPort:   23480,
		RegionID:   953,
		RegionCode: "e2e-derp",
		RegionName: "E2E Relay",
	}

	relay, err := NewDERPRelay(derpNodeKey, cfg)
	if err != nil {
		t.Fatalf("NewDERPRelay failed: %v", err)
	}
	if err := relay.Start(); err != nil {
		t.Fatalf("relay.Start failed: %v", err)
	}
	defer func() { _ = relay.Close() }()

	// 2. Start server listening via this DERP region
	serverKey := key.NewNode()
	receivedMsg := make(chan string, 1)

	server := &tailcat.Server{
		Key:    serverKey,
		Region: relay.DERPRegion(),
		Logf:   func(string, ...any) {},
		OnTCP: func(p uint16) func(net.Conn) {
			if p == 19999 {
				return func(c net.Conn) {
					defer func() { _ = c.Close() }()
					buf := make([]byte, 128)
					n, err := c.Read(buf)
					if err == nil {
						receivedMsg <- string(buf[:n])
						_, _ = c.Write([]byte("PONG-FROM-SERVER"))
					}
				}
			}
			return nil
		},
	}

	if err := server.Start(); err != nil {
		t.Fatalf("server.Start failed: %v", err)
	}
	defer func() { _ = server.Close() }()

	// 3. Client connects using the Tailcat address generated by the server (which embeds the private DERP)
	serverAddr := server.TailcatAddr()
	clientKey := key.NewNode()
	client := &tailcat.Client{
		Server: serverAddr,
		Key:    clientKey,
		Logf:   func(string, ...any) {},
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := client.DialTCPPort(ctx, 19999)
	if err != nil {
		t.Fatalf("client.DialTCPPort failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("PING-FROM-CLIENT")); err != nil {
		t.Fatalf("conn.Write failed: %v", err)
	}

	select {
	case msg := <-receivedMsg:
		if msg != "PING-FROM-CLIENT" {
			t.Fatalf("expected 'PING-FROM-CLIENT', got '%s'", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for server to receive message")
	}

	respBuf := make([]byte, 128)
	n, err := conn.Read(respBuf)
	if err != nil {
		t.Fatalf("conn.Read failed: %v", err)
	}
	if string(respBuf[:n]) != "PONG-FROM-SERVER" {
		t.Fatalf("expected 'PONG-FROM-SERVER', got '%s'", string(respBuf[:n]))
	}
}

