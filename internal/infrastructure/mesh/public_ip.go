package mesh

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"tailscale.com/net/stun"
)

// DetectPublicIP probes STUN and public reflection services to determine the node's WAN IP.
func DetectPublicIP(ctx context.Context) string {
	// 1. Try UDP STUN reflection (ultra-fast, ~10-50ms)
	stunServers := []string{
		"stun.cloudflare.com:3478",
		"stun.miwifi.com:3478",
		"stun.chat.bilibili.com:3478",
		"stun.syncthing.net:3478",
	}

	for _, srv := range stunServers {
		if ip := probeSTUNPublicIP(ctx, srv); ip != "" {
			return ip
		}
	}

	// 2. Fallback to HTTPS reflection endpoints
	httpEndpoints := []string{
		"https://api.ipify.org",
		"https://icanhazip.com",
	}

	for _, ep := range httpEndpoints {
		if ip := probeHTTPPublicIP(ctx, ep); ip != "" {
			return ip
		}
	}

	return ""
}

func probeSTUNPublicIP(ctx context.Context, server string) string {
	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()

	conn, err := d.DialContext(dialCtx, "udp", server)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))
	txID := stun.NewTxID()
	if _, err := conn.Write(stun.Request(txID)); err != nil {
		return ""
	}

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return ""
	}

	if !stun.Is(buf[:n]) {
		return ""
	}

	resTxID, addrPort, err := stun.ParseResponse(buf[:n])
	if err != nil || resTxID != txID {
		return ""
	}

	ip := addrPort.Addr()
	if ip.IsValid() && !ip.IsPrivate() && !ip.IsLoopback() {
		return ip.String()
	}
	return ""
}

func probeHTTPPublicIP(ctx context.Context, endpoint string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(string(b))
	parsed := net.ParseIP(raw)
	if parsed != nil && !parsed.IsPrivate() && !parsed.IsLoopback() {
		return parsed.String()
	}
	return ""
}
