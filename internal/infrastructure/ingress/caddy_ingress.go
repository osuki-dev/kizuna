package ingress

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// CaddyOptions allows using an existing Caddy installation. ConfigFile must import
// the managed Caddyfile; Kizuna never edits the external root configuration.
type CaddyOptions struct {
	ContainerName       string
	Image               string
	ConfigFile          string
	ContainerConfigFile string
}

// CaddyManager implements domain.IngressManager.
type CaddyManager struct {
	mu        sync.Mutex
	caddyDir  string
	caddyFile string
	routes    map[string]*entity.IngressConfig
	options   CaddyOptions
	loadErr   error
	run       func(context.Context, string, ...string) ([]byte, error)
	lookPath  func(string) (string, error)
}

func NewCaddyManager(baseDir string) domain.IngressManager {
	return NewCaddyManagerWithOptions(baseDir, CaddyOptions{
		ContainerName:       os.Getenv("KIZUNA_CADDY_CONTAINER"),
		Image:               os.Getenv("KIZUNA_CADDY_IMAGE"),
		ConfigFile:          os.Getenv("KIZUNA_CADDY_CONFIG"),
		ContainerConfigFile: os.Getenv("KIZUNA_CADDY_CONTAINER_CONFIG"),
	})
}

func NewCaddyManagerWithOptions(baseDir string, options CaddyOptions) domain.IngressManager {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna", "caddy")
	}
	mgr := &CaddyManager{caddyDir: baseDir, caddyFile: filepath.Join(baseDir, "Caddyfile"), routes: make(map[string]*entity.IngressConfig), options: options}
	mgr.loadErr = os.MkdirAll(baseDir, 0700)
	if mgr.loadErr == nil {
		mgr.loadErr = mgr.loadRoutes()
	}
	return mgr
}

const statePrefix = "# kizuna-managed-v1 "

func (m *CaddyManager) loadRoutes() error {
	content, err := os.ReadFile(m.caddyFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(content), statePrefix) {
		return fmt.Errorf("refusing to overwrite unmanaged Caddyfile %s", m.caddyFile)
	}
	line := strings.SplitN(string(content), "\n", 2)[0]
	state, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, statePrefix))
	if err != nil {
		return fmt.Errorf("invalid managed Caddy state: %w", err)
	}
	if err := json.Unmarshal(state, &m.routes); err != nil {
		return fmt.Errorf("invalid managed routes: %w", err)
	}
	if m.routes == nil {
		m.routes = make(map[string]*entity.IngressConfig)
	}
	for name, route := range m.routes {
		if route == nil || route.Domain != name {
			return fmt.Errorf("invalid persisted route %q", name)
		}
	}
	return os.Chmod(m.caddyFile, 0600)
}

func (m *CaddyManager) ConfigureRoute(ctx context.Context, route *entity.IngressConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return m.loadErr
	}
	if route == nil || route.Domain == "" || strings.ContainsAny(route.Domain, " \t\r\n{}") || (route.UpstreamPort <= 0 && len(route.Upstreams) == 0) {
		return fmt.Errorf("invalid ingress route: domain and upstream destination are required")
	}
	for _, upstream := range route.Upstreams {
		if err := entity.ValidateUpstreamAddress(upstream); err != nil {
			return fmt.Errorf("invalid ingress upstream %q: %w", upstream, err)
		}
	}
	// Take ownership of configuration so callers cannot mutate committed state.
	data, err := json.Marshal(route)
	if err != nil {
		return err
	}
	var copy entity.IngressConfig
	if err := json.Unmarshal(data, &copy); err != nil {
		return err
	}
	return m.updateRoutes(ctx, route.Domain, &copy)
}

func (m *CaddyManager) RemoveRoute(ctx context.Context, domainName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return m.loadErr
	}
	return m.updateRoutes(ctx, domainName, nil)
}

func (m *CaddyManager) updateRoutes(ctx context.Context, name string, route *entity.IngressConfig) error {
	oldRoutes := m.routes
	next := make(map[string]*entity.IngressConfig, len(oldRoutes))
	for k, v := range oldRoutes {
		next[k] = v
	}
	if route == nil {
		delete(next, name)
	} else {
		next[name] = route
	}
	oldFile, readErr := os.ReadFile(m.caddyFile)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	// Re-check ownership to protect files changed after construction.
	if readErr == nil && !strings.HasPrefix(string(oldFile), statePrefix) {
		return fmt.Errorf("refusing to overwrite unmanaged Caddyfile")
	}
	m.routes = next
	if err := m.writeCaddyfile(); err != nil {
		m.routes = oldRoutes
		return err
	}
	if err := m.reloadInternal(ctx); err != nil {
		m.routes = oldRoutes
		var restoreErr error
		if errors.Is(readErr, os.ErrNotExist) {
			restoreErr = os.Remove(m.caddyFile)
		} else {
			restoreErr = atomicWrite(m.caddyFile, oldFile)
		}
		if restoreErr != nil {
			return fmt.Errorf("%w; restoring Caddyfile failed: %v", err, restoreErr)
		}
		return err
	}
	return nil
}

func (m *CaddyManager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return m.loadErr
	}
	return m.reloadInternal(ctx)
}

func atomicWrite(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".kizuna-caddy-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
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
	state, err := json.Marshal(m.routes)
	if err != nil {
		return err
	}
	fmt.Fprintf(&sb, "%s%s\n", statePrefix, base64.StdEncoding.EncodeToString(state))
	if m.options.ConfigFile == "" {
		sb.WriteString("{\n")
		sb.WriteString("    admin 127.0.0.1:2019\n")
		sb.WriteString("    servers {\n")
		sb.WriteString("        protocols h1 h2 h3\n")
		sb.WriteString("    }\n")
		sb.WriteString("}\n\n")
	}

	// Deterministic domain ordering
	var domains []string
	for d := range m.routes {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	for _, domainName := range domains {
		route := m.routes[domainName]
		siteAddress := domainName
		if route.TLS == "none" && !strings.HasPrefix(siteAddress, "http://") {
			siteAddress = "http://" + siteAddress
		}
		fmt.Fprintf(&sb, "%s {\n", siteAddress)

		// 1. TLS Handling (LAN/Internal IP, Cloudflare DNS-01, internal CA, or custom)
		if route.TLS == "none" {
			// An explicit http:// site address disables automatic HTTPS.
		} else if route.TLS == "internal" || (route.TLS == "" && isPrivateOrLocal(domainName)) {
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

	return atomicWrite(m.caddyFile, []byte(sb.String()))
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

func (m *CaddyManager) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	if m.run != nil {
		return m.run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (m *CaddyManager) reloadInternal(ctx context.Context) error {
	lookup := m.lookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	config := m.caddyFile
	if m.options.ConfigFile != "" {
		config = m.options.ConfigFile
		content, err := os.ReadFile(config)
		if err != nil {
			return fmt.Errorf("read existing Caddy config: %w", err)
		}
		if !hasImport(string(content), m.caddyFile) && !hasImport(string(content), "/etc/caddy/kizuna/Caddyfile") {
			return fmt.Errorf("existing Caddy config must explicitly import managed Caddyfile")
		}
		if filepath.Clean(config) == filepath.Clean(m.caddyFile) {
			return fmt.Errorf("external Caddy config must differ from managed Caddyfile")
		}
	}
	checkModules := func(name string, prefix ...string) error {
		for _, route := range m.routes {
			if route.TLS == "none" || (route.TLS != "cloudflare" && route.DNSProvider != "cloudflare") {
				continue
			}
			out, err := m.command(ctx, name, append(prefix, "list-modules")...)
			if err != nil || !strings.Contains(string(out), "dns.providers.cloudflare") {
				return fmt.Errorf("cloudflare TLS requires a Caddy binary/image with dns.providers.cloudflare installed")
			}
			break
		}
		return nil
	}
	if m.options.ContainerName == "" {
		if _, err := lookup("caddy"); err == nil {
			if m.options.ConfigFile != "" {
				content, err := os.ReadFile(config)
				if err != nil {
					return err
				}
				if !hasImport(string(content), m.caddyFile) {
					return fmt.Errorf("local Caddy root config must import %s", m.caddyFile)
				}
			}
			if err := checkModules("caddy"); err != nil {
				return err
			}
			if _, err := m.command(ctx, "caddy", "validate", "--config", config, "--adapter", "caddyfile"); err != nil {
				return fmt.Errorf("caddy validation failed: %w", err)
			}
			if _, err := m.command(ctx, "caddy", "reload", "--config", config, "--adapter", "caddyfile"); err != nil {
				// Reload failure must not silently start a second server.
				return fmt.Errorf("caddy reload failed (start Caddy before publishing): %w", err)
			}
			return nil
		}
	}
	if _, err := lookup("docker"); err != nil {
		return fmt.Errorf("neither caddy binary nor docker found to run ingress reverse proxy")
	}
	container := m.options.ContainerName
	if container == "" {
		container = "kizuna-caddy"
	}
	out, err := m.command(ctx, "docker", "inspect", "--format", "{{.State.Running}}", container)
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		if m.options.ContainerName != "" || m.options.ConfigFile != "" {
			return fmt.Errorf("existing Caddy container %s is not running", container)
		}
		image := m.options.Image
		if image == "" {
			for _, route := range m.routes {
				if route.TLS != "none" && (route.TLS == "cloudflare" || route.DNSProvider == "cloudflare") {
					return fmt.Errorf("cloudflare TLS requires KIZUNA_CADDY_IMAGE with dns.providers.cloudflare installed")
				}
			}
			image = "caddy:latest"
		}
		// Validate with the selected image before starting a persistent container.
		if _, err := m.command(ctx, "docker", "run", "--rm", "-v", m.caddyDir+":/etc/caddy", image, "caddy", "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
			return fmt.Errorf("caddy image validation failed: %w", err)
		}
		_, err := m.command(ctx, "docker", "run", "-d", "--name", container, "--restart", "unless-stopped", "-p", "80:80", "-p", "443:443", "-p", "443:443/udp", "--log-opt", "max-size=50m", "--log-opt", "max-file=3", "-v", m.caddyDir+":/etc/caddy", "-v", "kizuna_caddy_data:/data", image)
		return err
	}
	if err := checkModules("docker", "exec", container, "caddy"); err != nil {
		return err
	}
	managedPath := "/etc/caddy/Caddyfile"
	rootPath := managedPath
	if m.options.ConfigFile != "" {
		// Reuse requires a directory bind mount so atomic replacement is visible,
		// and an explicit import of this file in the operator-owned root config.
		managedPath = "/etc/caddy/kizuna/Caddyfile"
		rootPath = m.options.ContainerConfigFile
		if rootPath == "" {
			return fmt.Errorf("existing Caddy requires ContainerConfigFile and an import of /etc/caddy/kizuna/Caddyfile")
		}
	}
	// Directory bind mounts are required: atomic replacement must be visible in
	// Caddy. Do not copy into an operator-owned configuration tree.
	hostContent, err := os.ReadFile(m.caddyFile)
	if err != nil {
		return err
	}
	mountedContent, err := m.command(ctx, "docker", "exec", container, "cat", managedPath)
	if err != nil || string(mountedContent) != string(hostContent) {
		return fmt.Errorf("caddy requires a directory bind mount exposing %s at %s", m.caddyDir, filepath.Dir(managedPath))
	}
	if m.options.ConfigFile != "" {
		rootContent, err := m.command(ctx, "docker", "exec", container, "cat", rootPath)
		if err != nil {
			return fmt.Errorf("read existing Caddy root config: %w", err)
		}
		if !hasImport(string(rootContent), managedPath) {
			return fmt.Errorf("existing Caddy root config must explicitly import %s", managedPath)
		}
	}
	if _, err := m.command(ctx, "docker", "exec", container, "caddy", "validate", "--config", rootPath, "--adapter", "caddyfile"); err != nil {
		return fmt.Errorf("caddy validation failed: %w", err)
	}
	if _, err := m.command(ctx, "docker", "exec", container, "caddy", "reload", "--config", rootPath, "--adapter", "caddyfile"); err != nil {
		return fmt.Errorf("caddy reload failed: %w", err)
	}
	return nil
}

func hasImport(config, path string) bool {
	for _, line := range strings.Split(config, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "import" && strings.Trim(fields[1], "\"") == path {
			return true
		}
	}
	return false
}
