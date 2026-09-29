package ingress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

func TestCaddyfileGenerationWithCustomizations(t *testing.T) {
	tempDir := t.TempDir()
	caddyFile := filepath.Join(tempDir, "Caddyfile")

	mgr := &CaddyManager{
		caddyDir:  tempDir,
		caddyFile: caddyFile,
		routes:    make(map[string]*entity.IngressConfig),
	}

	// 1. Configure simple route
	route1 := &entity.IngressConfig{
		Domain:       "simple.example.com",
		UpstreamPort: 8080,
	}
	mgr.routes[route1.Domain] = route1

	// 2. Configure advanced route with custom headers, proxy options, and custom directives
	route2 := &entity.IngressConfig{
		Domain:       "ai.example.com",
		UpstreamPort: 3000,
		Headers: &entity.IngressHeaders{
			Request: map[string]string{
				"X-Real-IP": "{remote_host}",
			},
			Response: map[string]string{
				"X-Frame-Options": "DENY",
				"X-Custom-Header": "KizunaProxy",
			},
		},
		ProxyOptions: &entity.ProxyOptions{
			FlushInterval:      "-1",
			InsecureSkipVerify: true,
			MaxBufferSize:      "4mb",
		},
		Custom: []string{
			"encode gzip zstd",
		},
	}
	mgr.routes[route2.Domain] = route2

	// 3. Configure multi-upstream load balanced route
	route3 := &entity.IngressConfig{
		Domain:    "lb.example.com",
		Upstreams: []string{"10.0.0.1:3000", "10.0.0.2:3000"},
		LBPolicy:  "least_conn",
	}
	mgr.routes[route3.Domain] = route3

	err := mgr.writeCaddyfile()
	if err != nil {
		t.Fatalf("failed to write Caddyfile: %v", err)
	}

	contentBytes, err := os.ReadFile(caddyFile)
	if err != nil {
		t.Fatalf("failed to read written Caddyfile: %v", err)
	}
	content := string(contentBytes)

	// Check protocols
	if !strings.Contains(content, "protocols h1 h2 h3") {
		t.Errorf("expected HTTP/3 protocols config in Caddyfile")
	}

	// Check route 1
	if !strings.Contains(content, "simple.example.com {\n    reverse_proxy 127.0.0.1:8080\n}") {
		t.Errorf("route 1 rendered incorrectly: %s", content)
	}

	// Check route 2 (advanced with headers, proxy options, and custom encode)
	if !strings.Contains(content, "ai.example.com {") {
		t.Errorf("missing ai.example.com in Caddyfile")
	}
	if !strings.Contains(content, "X-Frame-Options \"DENY\"") {
		t.Errorf("missing custom response header in Caddyfile")
	}
	if !strings.Contains(content, "encode gzip zstd") {
		t.Errorf("missing custom directive 'encode gzip zstd' in Caddyfile")
	}
	if !strings.Contains(content, "header_up X-Real-IP {remote_host}") {
		t.Errorf("missing header_up directive in Caddyfile")
	}
	if !strings.Contains(content, "flush_interval -1") {
		t.Errorf("missing flush_interval -1 (SSE/streaming) in Caddyfile")
	}
	if !strings.Contains(content, "tls_insecure_skip_verify") {
		t.Errorf("missing tls_insecure_skip_verify in Caddyfile")
	}

	// Check route 3 (multi-upstream load balancer)
	if !strings.Contains(content, "reverse_proxy 10.0.0.1:3000 10.0.0.2:3000 {") {
		t.Errorf("missing multi-upstream reverse_proxy in Caddyfile")
	}
	if !strings.Contains(content, "lb_policy least_conn") {
		t.Errorf("missing lb_policy least_conn in Caddyfile")
	}

	// Test RemoveRoute
	err = mgr.RemoveRoute(context.Background(), "simple.example.com")
	if err != nil && !strings.Contains(err.Error(), "neither caddy binary nor docker found") {
		t.Fatalf("RemoveRoute error: %v", err)
	}
	if _, ok := mgr.routes["simple.example.com"]; ok {
		t.Errorf("expected simple.example.com to be deleted from routes")
	}
}

func TestLANandHomelabHTTPSPresets(t *testing.T) {
	tempDir := t.TempDir()
	caddyFile := filepath.Join(tempDir, "Caddyfile")

	mgr := &CaddyManager{
		caddyDir:  tempDir,
		caddyFile: caddyFile,
		routes:    make(map[string]*entity.IngressConfig),
	}

	// 1. Private LAN IP route -> automatic "tls internal"
	mgr.routes["192.168.1.100"] = &entity.IngressConfig{
		Domain:       "192.168.1.100",
		UpstreamPort: 8000,
	}

	// 2. Homelab local domain -> automatic "tls internal"
	mgr.routes["pve.homelab.local"] = &entity.IngressConfig{
		Domain:       "pve.homelab.local",
		UpstreamPort: 8006,
	}

	// 3. Homelab public domain using Cloudflare DNS-01 ACME challenge
	mgr.routes["nas.mydomain.com"] = &entity.IngressConfig{
		Domain:       "nas.mydomain.com",
		UpstreamPort: 5000,
		TLS:          "cloudflare",
	}

	// 4. Modern Web App with WebSocket, SSE, gRPC, and CORS presets
	mgr.routes["app.example.com"] = &entity.IngressConfig{
		Domain:       "app.example.com",
		UpstreamPort: 3000,
		WebSocket:    true,
		SSE:          true,
		GRPC:         true,
		CORS:         true,
	}

	if err := mgr.writeCaddyfile(); err != nil {
		t.Fatalf("failed to write Caddyfile: %v", err)
	}

	contentBytes, err := os.ReadFile(caddyFile)
	if err != nil {
		t.Fatalf("failed to read Caddyfile: %v", err)
	}
	content := string(contentBytes)

	// Check LAN IP tls internal
	if !strings.Contains(content, "192.168.1.100 {\n    tls internal") {
		t.Errorf("expected 192.168.1.100 to automatically have 'tls internal':\n%s", content)
	}

	// Check .local tls internal
	if !strings.Contains(content, "pve.homelab.local {\n    tls internal") {
		t.Errorf("expected pve.homelab.local to automatically have 'tls internal':\n%s", content)
	}

	// 5. Test explicit Cloudflare API token
	mgr.routes["cf-explicit.example.com"] = &entity.IngressConfig{
		Domain:          "cf-explicit.example.com",
		UpstreamPort:    4000,
		TLS:             "cloudflare",
		CloudflareToken: "test_token_secret_12345",
	}

	if err := mgr.writeCaddyfile(); err != nil {
		t.Fatalf("failed to write Caddyfile: %v", err)
	}

	contentBytes2, _ := os.ReadFile(caddyFile)
	if !strings.Contains(string(contentBytes2), "dns cloudflare test_token_secret_12345") {
		t.Errorf("expected explicit Cloudflare API token in Caddyfile:\n%s", string(contentBytes2))
	}
}
