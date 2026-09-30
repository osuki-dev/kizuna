package mesh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/gossip"
	"tailscale.com/derp/derpserver"
	"tailscale.com/net/stun"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

type derpCertFile struct {
	CertPEM     string `json:"cert_pem"`
	KeyPEM      string `json:"key_pem"`
	Fingerprint string `json:"fingerprint"`
}

func loadOrCreateDERPCert(host string) (tls.Certificate, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	keyDir := filepath.Join(home, ".kizuna")
	_ = os.MkdirAll(keyDir, 0700)
	certFile := filepath.Join(keyDir, "derp_cert.json")

	if data, err := os.ReadFile(certFile); err == nil {
		var cf derpCertFile
		if err := json.Unmarshal(data, &cf); err == nil && cf.CertPEM != "" && cf.KeyPEM != "" {
			cert, err := tls.X509KeyPair([]byte(cf.CertPEM), []byte(cf.KeyPEM))
			if err == nil {
				return cert, cf.Fingerprint, nil
			}
		}
	}

	// Generate persistent self-signed certificate
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("failed to generate rsa key: %w", err)
	}

	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Kizuna Private DERP Relay"},
			CommonName:   host,
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip, net.ParseIP("127.0.0.1")}
	} else if host != "" {
		template.DNSNames = []string{host, "localhost"}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("failed to create certificate: %w", err)
	}

	h := sha256.Sum256(certDER)
	fingerprint := "sha256-raw:" + hex.EncodeToString(h[:])

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	cf := derpCertFile{
		CertPEM:     string(certPEM),
		KeyPEM:      string(keyPEM),
		Fingerprint: fingerprint,
	}
	if b, err := json.MarshalIndent(cf, "", "  "); err == nil {
		_ = os.WriteFile(certFile, b, 0600)
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("failed to load generated keypair: %w", err)
	}

	return tlsCert, fingerprint, nil
}

// DERPRelay runs an embedded private DERP server and optional STUN responder
type DERPRelay struct {
	mu          sync.Mutex
	cfg         entity.DERPConfig
	nodeKey     key.NodePrivate
	tlsCert     tls.Certificate
	fingerprint string
	server      *derpserver.Server
	listener    net.Listener
	httpServer  *http.Server
	stunConn    *net.UDPConn
	isClosed    bool
}

// NewDERPRelay creates an initialized private DERP relay
func NewDERPRelay(nodeKey key.NodePrivate, cfg entity.DERPConfig) (*DERPRelay, error) {
	if cfg.Port <= 0 {
		cfg.Port = 8443
	}
	if cfg.STUNPort < 0 {
		cfg.STUNPort = 0
	} else if cfg.STUNPort == 0 {
		cfg.STUNPort = 3478
	}
	if cfg.RegionID <= 0 {
		cfg.RegionID = 901
	}
	if cfg.RegionCode == "" {
		cfg.RegionCode = "kzn"
	}
	if cfg.RegionName == "" {
		cfg.RegionName = "Kizuna Private Relay"
	}
	if cfg.Host == "" {
		cfg.Host = gossip.DetectOutboundIP()
		if cfg.Host == "" {
			cfg.Host = "127.0.0.1"
		}
	}

	tlsCert, fingerprint, err := loadOrCreateDERPCert(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("derp relay cert setup failed: %w", err)
	}

	return &DERPRelay{
		cfg:         cfg,
		nodeKey:     nodeKey,
		tlsCert:     tlsCert,
		fingerprint: fingerprint,
	}, nil
}

// Start launches the TLS DERP HTTP server and optional STUN responder
func (r *DERPRelay) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isClosed {
		return fmt.Errorf("derp relay is already closed")
	}

	logf := func(string, ...any) {}
	if os.Getenv("KIZUNA_DEBUG") != "" {
		logf = func(format string, args ...any) {
			fmt.Printf("[DERP] "+format+"\n", args...)
		}
	}

	// 1. Initialize derpserver.Server
	dServer := derpserver.New(r.nodeKey, logf)
	r.server = dServer

	mux := http.NewServeMux()
	mux.Handle("/derp", derpserver.Handler(dServer))
	mux.HandleFunc("/derp/probe", derpserver.ProbeHandler)
	mux.HandleFunc("/derp/latency-check", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// 2. Bind TCP listener on configured port
	tcpLn, err := net.Listen("tcp", fmt.Sprintf(":%d", r.cfg.Port))
	if err != nil {
		return fmt.Errorf("failed to bind DERP TCP port :%d: %w", r.cfg.Port, err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{r.tlsCert},
		MinVersion:   tls.VersionTLS12,
	}
	tlsLn := tls.NewListener(tcpLn, tlsConfig)
	r.listener = tlsLn

	httpServer := &http.Server{
		Handler:     mux,
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 120 * time.Second,
		TLSConfig:   tlsConfig,
	}
	r.httpServer = httpServer

	go func() {
		_ = httpServer.Serve(tlsLn)
	}()

	// 3. Optional STUN server on UDP
	if r.cfg.STUNPort > 0 {
		udpConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: r.cfg.STUNPort})
		if err == nil {
			r.stunConn = udpConn
			go r.serveSTUN(udpConn)
		} else {
			// If STUN binding failed (e.g. port taken), do not advertise dead STUN port
			r.cfg.STUNPort = 0
		}
	}

	return nil
}

func (r *DERPRelay) serveSTUN(conn *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		n, rAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := buf[:n]
		if !stun.Is(pkt) {
			continue
		}
		txID, err := stun.ParseBindingRequest(pkt)
		if err != nil {
			continue
		}
		ap := rAddr.AddrPort()
		resp := stun.Response(txID, ap)
		_, _ = conn.WriteToUDP(resp, rAddr)
	}
}

// Close gracefully stops the DERP server and listeners
func (r *DERPRelay) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isClosed {
		return nil
	}
	r.isClosed = true

	var firstErr error
	if r.httpServer != nil {
		if err := r.httpServer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	} else if r.listener != nil {
		if err := r.listener.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if r.stunConn != nil {
		if err := r.stunConn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if r.server != nil {
		if err := r.server.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// DERPRegion constructs a tailcfg.DERPRegion representation of this relay
func (r *DERPRelay) DERPRegion() *tailcfg.DERPRegion {
	return BuildDERPRegion(r.NodeInfo())
}

// NodeInfo returns the gossip-advertisable metadata for this relay
func (r *DERPRelay) NodeInfo() *entity.DERPNodeInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	return &entity.DERPNodeInfo{
		RegionID:   r.cfg.RegionID,
		RegionCode: r.cfg.RegionCode,
		RegionName: r.cfg.RegionName,
		HostName:   r.cfg.Host,
		Port:       r.cfg.Port,
		STUNPort:   r.cfg.STUNPort,
		CertName:   r.fingerprint,
		IPv4:       r.cfg.Host,
	}
}

// BuildDERPRegion converts entity.DERPNodeInfo into tailcfg.DERPRegion
func BuildDERPRegion(info *entity.DERPNodeInfo) *tailcfg.DERPRegion {
	if info == nil {
		return nil
	}
	regID := tailcfg.DERPRegionID(info.RegionID)
	nodeName := fmt.Sprintf("%da", info.RegionID)

	return &tailcfg.DERPRegion{
		RegionID:   regID,
		RegionCode: info.RegionCode,
		RegionName: info.RegionName,
		Nodes: []*tailcfg.DERPNode{
			{
				Name:     nodeName,
				RegionID: regID,
				HostName: info.HostName,
				DERPPort: info.Port,
				STUNPort: info.STUNPort,
				CertName: info.CertName,
				IPv4:     info.IPv4,
				IPv6:     info.IPv6,
			},
		},
	}
}

// MeasureDERPLatency measures the round-trip time (RTT) to a DERP relay
func MeasureDERPLatency(node *entity.DERPNodeInfo, timeout time.Duration) (time.Duration, error) {
	if node == nil || node.HostName == "" {
		return 0, fmt.Errorf("empty host name")
	}

	// 1. Try STUN ping if STUN port is defined
	if node.STUNPort > 0 {
		rtt, err := probeSTUN(node.HostName, node.STUNPort, timeout)
		if err == nil {
			return rtt, nil
		}
	}

	// 2. Fallback to TCP TLS dial latency
	start := time.Now()
	target := net.JoinHostPort(node.HostName, strconv.Itoa(node.Port))
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.Dial("tcp", target)
	if err == nil {
		_ = conn.Close()
		return time.Since(start), nil
	}

	return 0, fmt.Errorf("derp probe failed: %w", err)
}

func probeSTUN(host string, port int, timeout time.Duration) (time.Duration, error) {
	conn, err := net.DialTimeout("udp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	txID := stun.NewTxID()
	req := stun.Request(txID)

	start := time.Now()
	if _, err := conn.Write(req); err != nil {
		return 0, err
	}

	respBuf := make([]byte, 1024)
	n, err := conn.Read(respBuf)
	if err != nil {
		return 0, err
	}

	if !stun.Is(respBuf[:n]) {
		return 0, fmt.Errorf("not stun packet")
	}

	resTxID, _, err := stun.ParseResponse(respBuf[:n])
	if err != nil || resTxID != txID {
		return 0, fmt.Errorf("stun tx mismatch: %v", err)
	}

	return time.Since(start), nil
}

// PickBestDERP concurrently benchmarks candidate DERP relays and selects the fastest one
func PickBestDERP(ctx context.Context, candidates []*entity.DERPNodeInfo, timeout time.Duration) *entity.DERPNodeInfo {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	type result struct {
		node *entity.DERPNodeInfo
		rtt  time.Duration
		err  error
	}

	resCh := make(chan result, len(candidates))
	var wg sync.WaitGroup

	for _, c := range candidates {
		wg.Add(1)
		go func(target *entity.DERPNodeInfo) {
			defer wg.Done()
			rtt, err := MeasureDERPLatency(target, timeout)
			resCh <- result{node: target, rtt: rtt, err: err}
		}(c)
	}

	wg.Wait()
	close(resCh)

	var best *entity.DERPNodeInfo
	minRTT := 24 * time.Hour

	for res := range resCh {
		if res.err == nil && res.rtt < minRTT {
			minRTT = res.rtt
			best = res.node
		}
	}

	if best == nil {
		return candidates[0]
	}

	return best
}
