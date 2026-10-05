package ingress

import (
	"context"
	"fmt"
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

	mgr.lookPath = func(string) (string, error) { return "mock", nil }
	mgr.run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
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

func mockLocalManager(t *testing.T) *CaddyManager {
	t.Helper()
	mgr := NewCaddyManagerWithOptions(t.TempDir(), CaddyOptions{}).(*CaddyManager)
	mgr.lookPath = func(string) (string, error) { return "mock", nil }
	mgr.run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	return mgr
}

func TestRoutesSurviveRestartAndRemove(t *testing.T) {
	mgr := mockLocalManager(t)
	route := &entity.IngressConfig{Domain: "one.example.com", UpstreamPort: 8080, CloudflareToken: "sensitive"}
	if err := mgr.ConfigureRoute(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	route.UpstreamPort = 9000
	recovered := NewCaddyManagerWithOptions(mgr.caddyDir, CaddyOptions{}).(*CaddyManager)
	recovered.lookPath, recovered.run = mgr.lookPath, mgr.run
	if got := recovered.routes[route.Domain].UpstreamPort; got != 8080 {
		t.Fatalf("persisted port = %d", got)
	}
	if err := recovered.ConfigureRoute(context.Background(), &entity.IngressConfig{Domain: "two.example.com", UpstreamPort: 8081}); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(mgr.caddyFile)
	if !strings.Contains(string(content), "one.example.com {") || !strings.Contains(string(content), "two.example.com {") {
		t.Fatal("old route lost after restart")
	}
	info, _ := os.Stat(mgr.caddyFile)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("sensitive file permissions = %o", info.Mode().Perm())
	}
	if err := recovered.RemoveRoute(context.Background(), route.Domain); err != nil {
		t.Fatal(err)
	}
	last := NewCaddyManagerWithOptions(mgr.caddyDir, CaddyOptions{}).(*CaddyManager)
	if _, ok := last.routes[route.Domain]; ok {
		t.Fatal("removed route persisted")
	}
}

func TestFailedUpdateRestoresRoutesAndFile(t *testing.T) {
	for _, phase := range []string{"validate", "reload"} {
		t.Run(phase, func(t *testing.T) {
			mgr := mockLocalManager(t)
			route := &entity.IngressConfig{Domain: "one.example.com", UpstreamPort: 8080}
			if err := mgr.ConfigureRoute(context.Background(), route); err != nil {
				t.Fatal(err)
			}
			old, _ := os.ReadFile(mgr.caddyFile)
			var commands []string
			mgr.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				commands = append(commands, args[0])
				if args[0] == phase {
					return nil, fmt.Errorf("mock failure")
				}
				return nil, nil
			}
			if err := mgr.RemoveRoute(context.Background(), route.Domain); err == nil {
				t.Fatal("expected failure")
			}
			restored, _ := os.ReadFile(mgr.caddyFile)
			if string(old) != string(restored) || mgr.routes[route.Domain] == nil {
				t.Fatal("failed update changed committed state")
			}
			if phase == "validate" && strings.Contains(strings.Join(commands, " "), "reload") {
				t.Fatal("reload after validation failure")
			}
			for _, command := range commands {
				if command == "start" {
					t.Fatal("failed reload attempted to start another server")
				}
			}
		})
	}
}

func TestUnmanagedAndCorruptFilesAreProtected(t *testing.T) {
	for _, content := range []string{"operator.example.com { reverse_proxy localhost:8080 }", statePrefix + "broken"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "Caddyfile")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		mgr := NewCaddyManagerWithOptions(dir, CaddyOptions{}).(*CaddyManager)
		if err := mgr.ConfigureRoute(context.Background(), &entity.IngressConfig{Domain: "app.example.com", UpstreamPort: 8080}); err == nil {
			t.Fatal("expected load failure")
		}
		after, _ := os.ReadFile(path)
		if string(after) != content {
			t.Fatal("existing file overwritten")
		}
	}
}

func TestCloudflareRequiresModule(t *testing.T) {
	mgr := mockLocalManager(t)
	mgr.run = func(context.Context, string, ...string) ([]byte, error) { return []byte("http.reverse_proxy"), nil }
	err := mgr.ConfigureRoute(context.Background(), &entity.IngressConfig{Domain: "app.example.com", TLS: "cloudflare", UpstreamPort: 8080})
	if err == nil || !strings.Contains(err.Error(), "dns.providers.cloudflare") {
		t.Fatalf("missing module error: %v", err)
	}
	if len(mgr.routes) != 0 {
		t.Fatal("failed route committed")
	}
	if _, err := os.Stat(mgr.caddyFile); !os.IsNotExist(err) {
		t.Fatal("failed initial config persisted")
	}
}

func TestExistingCaddyRequiresImportAndDoesNotEditRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "operator.Caddyfile")
	content := "operator.example.com {\n reverse_proxy localhost:9000\n}\nimport " + filepath.Join(dir, "Caddyfile") + "\n"
	if err := os.WriteFile(root, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := NewCaddyManagerWithOptions(dir, CaddyOptions{ConfigFile: root}).(*CaddyManager)
	mgr.lookPath = func(string) (string, error) { return "mock", nil }
	mgr.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) < 3 || args[2] != root {
			t.Fatalf("expected full root config: %v", args)
		}
		return nil, nil
	}
	if err := mgr.ConfigureRoute(context.Background(), &entity.IngressConfig{Domain: "app.example.com", UpstreamPort: 8080}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(root)
	if string(after) != content {
		t.Fatal("operator config changed")
	}
	managed, _ := os.ReadFile(mgr.caddyFile)
	if strings.Contains(string(managed), "admin 127.0.0.1") {
		t.Fatal("import fragment includes global config")
	}
}

func TestDockerReloadFailurePreservesCommittedConfiguration(t *testing.T) {
	mgr := mockLocalManager(t)
	original := &entity.IngressConfig{Domain: "app.example.com", UpstreamPort: 8080}
	if err := mgr.ConfigureRoute(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(mgr.caddyFile)
	mgr.options.ContainerName = "existing"
	mgr.run = func(_ context.Context, command string, args ...string) ([]byte, error) {
		if command != "docker" {
			t.Fatalf("unexpected command %s", command)
		}
		if args[0] == "inspect" {
			return []byte("true"), nil
		}
		if args[0] == "exec" && args[2] == "cat" {
			return os.ReadFile(mgr.caddyFile)
		}
		if args[0] == "exec" && args[2] == "caddy" {
			if args[3] == "reload" {
				return nil, fmt.Errorf("mock reload failure")
			}
			return nil, nil
		}
		t.Fatalf("unexpected docker command %v", args)
		return nil, nil
	}
	if err := mgr.ConfigureRoute(context.Background(), &entity.IngressConfig{Domain: original.Domain, UpstreamPort: 9000}); err == nil {
		t.Fatal("expected reload failure")
	}
	after, _ := os.ReadFile(mgr.caddyFile)
	if string(after) != string(old) || mgr.routes[original.Domain].UpstreamPort != 8080 {
		t.Fatal("failed Docker reload changed committed configuration")
	}
}

func TestDockerCloudflareNeverUsesDefaultImage(t *testing.T) {
	mgr := mockLocalManager(t)
	mgr.lookPath = func(name string) (string, error) {
		if name == "caddy" {
			return "", fmt.Errorf("not found")
		}
		return "mock", nil
	}
	mgr.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] != "inspect" {
			t.Fatalf("unexpected Docker invocation: %v", args)
		}
		return nil, fmt.Errorf("container missing")
	}
	err := mgr.ConfigureRoute(context.Background(), &entity.IngressConfig{Domain: "app.example.com", TLS: "cloudflare", UpstreamPort: 8080})
	if err == nil || !strings.Contains(err.Error(), "KIZUNA_CADDY_IMAGE") {
		t.Fatalf("expected explicit image requirement, got %v", err)
	}
}

func TestTLSNoneExplicitlyDisablesAutomaticHTTPS(t *testing.T) {
	mgr := mockLocalManager(t)
	if err := mgr.ConfigureRoute(context.Background(), &entity.IngressConfig{Domain: "plain.example.com", UpstreamPort: 8080, TLS: "none", DNSProvider: "cloudflare"}); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(mgr.caddyFile)
	if !strings.Contains(string(content), "http://plain.example.com {") {
		t.Fatal("TLS none still allows automatic HTTPS")
	}
	if strings.Contains(string(content), "    tls") {
		t.Fatal("TLS none emitted a TLS directive")
	}
}

func TestIngressBoundaryRejectsInvalidUpstreams(t *testing.T) {
	for _, upstream := range []string{"localhost:8080\n}\nmalicious.example.com {", "http://user:pass@localhost:8080", "http://localhost:8080/path", "localhost:0"} {
		mgr := mockLocalManager(t)
		mgr.run = func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("invalid upstream reached Caddy")
			return nil, nil
		}
		if err := mgr.ConfigureRoute(context.Background(), &entity.IngressConfig{Domain: "app.example.com", Upstreams: []string{upstream}}); err == nil {
			t.Fatalf("accepted invalid upstream %q", upstream)
		}
		if len(mgr.routes) != 0 {
			t.Fatal("invalid upstream changed committed routes")
		}
	}
	mgr := mockLocalManager(t)
	if err := mgr.ConfigureRoute(context.Background(), &entity.IngressConfig{Domain: "app.example.com", Upstreams: []string{"h2c://[::1]:8080"}}); err != nil {
		t.Fatal(err)
	}
}
