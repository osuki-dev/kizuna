package ingress

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// CaddyManager implements domain.IngressManager
type CaddyManager struct {
	mu        sync.Mutex
	caddyDir  string
	caddyFile string
	routes    map[string]*entity.IngressConfig // domain -> config
}

// NewCaddyManager initializes Caddy ingress controller
func NewCaddyManager(baseDir string) domain.IngressManager {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna", "caddy")
	}
	_ = os.MkdirAll(baseDir, 0755)

	mgr := &CaddyManager{
		caddyDir:  baseDir,
		caddyFile: filepath.Join(baseDir, "Caddyfile"),
		routes:    make(map[string]*entity.IngressConfig),
	}
	return mgr
}

// ConfigureRoute registers or updates a domain route
func (m *CaddyManager) ConfigureRoute(ctx context.Context, route *entity.IngressConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if route.Domain == "" || (route.UpstreamPort <= 0 && len(route.Upstreams) == 0) {
		return fmt.Errorf("invalid ingress route: domain and upstream destination are required")
	}

	m.routes[route.Domain] = route
	if err := m.writeCaddyfile(); err != nil {
		return err
	}

	return m.reloadInternal(ctx)
}

// RemoveRoute deletes a route by domain
func (m *CaddyManager) RemoveRoute(ctx context.Context, domainName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.routes, domainName)
	if err := m.writeCaddyfile(); err != nil {
		return err
	}
	return m.reloadInternal(ctx)
}

// Reload forces Caddy to reload configuration
func (m *CaddyManager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadInternal(ctx)
}

func isPrivateOrLocal(host string) bool {
	host = strings.Split(host, ":")[0]
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".home") {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
	}
	return false
}

func (m *CaddyManager) writeCaddyfile() error {
	var sb strings.Builder
	sb.WriteString("{\n")
	sb.WriteString("    admin 127.0.0.1:2019\n")
	sb.WriteString("    servers {\n")
	sb.WriteString("        protocols h1 h2 h3\n")
	sb.WriteString("    }\n")
	sb.WriteString("}\n\n")

	// Deterministic domain ordering
	var domains []string
	for d := range m.routes {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	for _, domainName := range domains {
		route := m.routes[domainName]
		fmt.Fprintf(&sb, "%s {\n", domainName)

		// 1. TLS Handling (LAN/Internal IP, Cloudflare DNS-01, internal CA, or custom)
		if route.TLS == "internal" || (route.TLS == "" && isPrivateOrLocal(domainName)) {
			sb.WriteString("    tls internal\n")
		} else if route.TLS == "cloudflare" || route.DNSProvider == "cloudflare" {
			token := route.CloudflareToken
			if token == "" {
				token = route.DNSToken
			}
			sb.WriteString("    tls {\n")
			if token != "" {
				fmt.Fprintf(&sb, "        dns cloudflare %s\n", token)
			} else {
				sb.WriteString("        dns cloudflare {env.CLOUDFLARE_API_TOKEN}\n")
			}
			sb.WriteString("    }\n")
		} else if route.TLS != "" && route.TLS != "auto" && route.TLS != "none" {
			fmt.Fprintf(&sb, "    tls %s\n", route.TLS)
		}

		// 2. CORS Header Preset
		if route.CORS {
			sb.WriteString("    header {\n")
			sb.WriteString("        Access-Control-Allow-Origin *\n")
			sb.WriteString("        Access-Control-Allow-Methods \"GET, POST, PUT, DELETE, OPTIONS, PATCH\"\n")
			sb.WriteString("        Access-Control-Allow-Headers \"Content-Type, Authorization, X-Requested-With\"\n")
			sb.WriteString("    }\n")
		}

		// 3. Custom response headers
		if route.Headers != nil && len(route.Headers.Response) > 0 {
			sb.WriteString("    header {\n")
			var respKeys []string
			for k := range route.Headers.Response {
				respKeys = append(respKeys, k)
			}
			sort.Strings(respKeys)
			for _, k := range respKeys {
				fmt.Fprintf(&sb, "        %s \"%s\"\n", k, route.Headers.Response[k])
			}
			sb.WriteString("    }\n")
		}

		// 4. Custom top-level directives (e.g. encode, basic_auth)
		for _, line := range route.Custom {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				fmt.Fprintf(&sb, "    %s\n", trimmed)
			}
		}

		// 5. Reverse proxy configuration
		hasProxyInner := route.SSE || route.WebSocket || route.GRPC ||
			(route.Headers != nil && len(route.Headers.Request) > 0) ||
			(route.ProxyOptions != nil && (route.ProxyOptions.FlushInterval != "" || route.ProxyOptions.InsecureSkipVerify || route.ProxyOptions.MaxBufferSize != ""))

		if len(route.Upstreams) > 1 {
			lbPolicy := route.LBPolicy
			if lbPolicy == "" {
				lbPolicy = "round_robin"
			}
			fmt.Fprintf(&sb, "    reverse_proxy %s {\n", strings.Join(route.Upstreams, " "))
			fmt.Fprintf(&sb, "        lb_policy %s\n", lbPolicy)
			fmt.Fprintf(&sb, "        lb_try_duration 5s\n")
			writeProxyDirectives(&sb, route)
			fmt.Fprintf(&sb, "    }\n")
		} else if hasProxyInner {
			upstream := fmt.Sprintf("127.0.0.1:%d", route.UpstreamPort)
			if len(route.Upstreams) == 1 {
				upstream = route.Upstreams[0]
			}
			fmt.Fprintf(&sb, "    reverse_proxy %s {\n", upstream)
			writeProxyDirectives(&sb, route)
			fmt.Fprintf(&sb, "    }\n")
		} else if len(route.Upstreams) == 1 {
			fmt.Fprintf(&sb, "    reverse_proxy %s\n", route.Upstreams[0])
		} else {
			fmt.Fprintf(&sb, "    reverse_proxy 127.0.0.1:%d\n", route.UpstreamPort)
		}
		sb.WriteString("}\n\n")
	}

	return os.WriteFile(m.caddyFile, []byte(sb.String()), 0644)
}

func writeProxyDirectives(sb *strings.Builder, route *entity.IngressConfig) {
	if route.SSE {
		fmt.Fprintf(sb, "        flush_interval -1\n")
	}

	if route.WebSocket {
		sb.WriteString("        header_up Upgrade {>Upgrade}\n")
		sb.WriteString("        header_up Connection {>Connection}\n")
	}

	if route.GRPC {
		sb.WriteString("        transport http {\n")
		sb.WriteString("            versions h2c 2\n")
		sb.WriteString("        }\n")
	}

	if route.Headers != nil && len(route.Headers.Request) > 0 {
		var reqKeys []string
		for k := range route.Headers.Request {
			reqKeys = append(reqKeys, k)
		}
		sort.Strings(reqKeys)
		for _, k := range reqKeys {
			fmt.Fprintf(sb, "        header_up %s %s\n", k, route.Headers.Request[k])
		}
	}

	if route.ProxyOptions != nil {
		if route.ProxyOptions.FlushInterval != "" && !route.SSE {
			fmt.Fprintf(sb, "        flush_interval %s\n", route.ProxyOptions.FlushInterval)
		}
		if route.ProxyOptions.MaxBufferSize != "" {
			fmt.Fprintf(sb, "        buffer_requests %s\n", route.ProxyOptions.MaxBufferSize)
		}
		if route.ProxyOptions.InsecureSkipVerify && !route.GRPC {
			sb.WriteString("        transport http {\n")
			sb.WriteString("            tls_insecure_skip_verify\n")
			sb.WriteString("        }\n")
		}
	}
}

func (m *CaddyManager) reloadInternal(ctx context.Context) error {
	// 1. Try local caddy binary if present in PATH
	if _, err := exec.LookPath("caddy"); err == nil {
		cmd := exec.CommandContext(ctx, "caddy", "reload", "--config", m.caddyFile)
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else {
			// If reload failed because caddy wasn't running, start it in background
			_ = exec.Command("caddy", "start", "--config", m.caddyFile).Start()
			_ = out
			return nil
		}
	}

	// 2. Fallback: run or reload Caddy container via Docker
	if _, err := exec.LookPath("docker"); err == nil {
		// Check if kizuna-caddy container exists
		check := exec.Command("docker", "ps", "-q", "-f", "name=kizuna-caddy").Output
		if out, err := check(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
			// Reload inside container
			copyCmd := exec.Command("docker", "cp", m.caddyFile, "kizuna-caddy:/etc/caddy/Caddyfile")
			_ = copyCmd.Run()
			return exec.Command("docker", "exec", "kizuna-caddy", "caddy", "reload", "--config", "/etc/caddy/Caddyfile").Run()
		}

		// Container not running, launch it with HTTP/3 (QUIC) and log bounds
		runCmd := exec.Command("docker", "run", "-d",
			"--name", "kizuna-caddy",
			"--restart", "unless-stopped",
			"-p", "80:80",
			"-p", "443:443",
			"-p", "443:443/udp",
			"--log-opt", "max-size=50m",
			"--log-opt", "max-file=3",
			"-v", fmt.Sprintf("%s:/etc/caddy/Caddyfile", m.caddyFile),
			"-v", "kizuna_caddy_data:/data",
			"caddy:latest",
		)
		return runCmd.Run()
	}

	return fmt.Errorf("neither caddy binary nor docker found to run ingress reverse proxy")
}
