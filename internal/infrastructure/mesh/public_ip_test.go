package mesh

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestDetectPublicIP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	ip := DetectPublicIP(ctx)
	// If online and connected to internet, ip should be valid IPv4 and not private
	if ip != "" {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			t.Fatalf("expected valid IP, got %q", ip)
		}
		if parsed.IsPrivate() || parsed.IsLoopback() {
			t.Fatalf("expected public IP, got private/loopback %q", ip)
		}
	}
}
