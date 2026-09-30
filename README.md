<p align="center">
  <img src="assets/kizuna_logo.png" alt="Kizuna Logo" width="200" style="border-radius: 20px;" />
</p>

<h1 align="center">Kizuna (絆)</h1>

<p align="center">
  <strong>Zero-Trust Mesh Deployment, Horizontal Scaling & Website Ingress CLI</strong><br>
  Built with Tailscale Tailcat, Caddy (HTTP/3), and Charm Lipgloss & Bubble Tea.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.23-00ADD8?style=flat-square&logo=go" alt="Go Version" />
  <img src="https://img.shields.io/badge/License-MIT-blue?style=flat-square" alt="License" />
  <img src="https://img.shields.io/badge/HTTP%2F3-QUIC%20Enabled-4E5EE4?style=flat-square" alt="HTTP/3" />
  <img src="https://img.shields.io/badge/Architecture-Clean-success?style=flat-square" alt="Clean Architecture" />
  <img src="https://img.shields.io/badge/i18n-en%20%7C%20ja%20%7C%20zh%20%2B%20dynamic-orange?style=flat-square" alt="i18n" />
</p>

---

## ⚡ Quick Install

### Linux & macOS (POSIX Shell)
```bash
curl -fsSL https://raw.githubusercontent.com/osuki-dev/kizuna/main/install.sh | bash
```

### Windows (PowerShell)
```powershell
irm https://raw.githubusercontent.com/osuki-dev/kizuna/main/install.ps1 | iex
```

*The installer automatically detects your OS and architecture, verifies SHA256 cryptographic checksums against `checksums.txt`, installs the binary, and prompts whether to configure it as an auto-starting system daemon.*

---

## 🌟 Overview

**Kizuna (絆)** is a zero-trust, peer-to-peer deployment tool and website publisher. It allows developers to deploy websites, microservices, and databases across remote servers, home PCs (Homelab / Proxmox VE), or edge devices without needing public IPs, open router firewall ports, or centralized VPN accounts.

### Key Highlights
* **Zero-Config P2P Mesh**: Powered by `tailscale/tailcat`. Direct encrypted WireGuard tunnels via STUN/UDP hole-punching and DERP fallback in pure userspace.
* **Decentralized Private DERP Relay**: Host embedded zero-config DERP relays on nodes (`--derp`) for NAT traversal, automatically discovered via gossip with dynamic latency benchmarking.
* **Agentless Scale-Out**: Deploy and horizontally scale across multiple production servers using pure SSH + Docker without installing Kizuna on worker nodes.
* **Caddy Ingress & HTTP/3**: Automated Let's Encrypt / ZeroSSL, Cloudflare DNS-01 ACME, or internal Root CA for LAN/Homelab IPs, with HTTP/3 (QUIC) and HTTP/2.
* **Common Proxy Presets**: One-click configuration for WebSockets, SSE & LLM streaming (`flush_interval -1`), gRPC (`h2c`), and CORS headers.
* **Production Operations Lifecycle**:
  * **Publish (`kizuna deploy`)**: Zero-downtime deploy with release history tracking.
  * **Scale (`kizuna scale <svc> <N>`)**: One-click horizontal replica scaling with dynamic Caddy load balancer pooling (`round_robin`, `least_conn`).
  * **Rollback (`kizuna rollback [svc]`)**: Instant one-click rollback to the previous stable revision.
  * **Backup (`kizuna backup`)**: Respects `.gitignore`, supports DB dumps (PostgreSQL, MySQL, SQLite, custom), and automated retention pruning (`keep_days`, `max_backups`).
* **Disk Exhaustion Protection**: Docker container logs capped at 150MB with automatic log rotation (`--log-opt max-size=50m --log-opt max-file=3`).
* **Real-time TUI Dashboard**: Terminal UI with CPU/Memory sparkline historical trends, load average, and telemetry gauges.
* **Extensible i18n**: Fully localized in English (`en`), Japanese (`ja`), and Chinese (`zh`), with automatic runtime discovery for new languages (`locales/*.json`) without recompiling.

---

## 🚀 Quickstart

### 1. Start Service Daemon on Host / Home Server
```bash
# Run in foreground (displays Mesh Address and a 6-digit Pairing PIN)
kizuna service run

# Or run with embedded private DERP relay (optional, for NAT traversal relaying):
kizuna service run --derp --derp-port 8443

# Or install as an auto-starting system service (systemd / launchd / Windows Service):
kizuna service install
```

### 2. Pair from Laptop or CI Machine
```bash
# Connect to target using its address and the 6-digit PIN
kizuna node add <mesh-address> --pin <6-digit-pin> --name home-server

# List configured and paired nodes
kizuna node list
```

### 3. Initialize & Validate Project Config
```bash
# Auto-detects Dockerfile, Compose, or Bun, and creates kizuna.yaml with multi-env presets
kizuna init

# Validate configuration file before deploying
kizuna check
```

### 4. Deploy, Scale & Rollback
```bash
# Deploy to active or specific environment (e.g. staging, production)
kizuna deploy -e production

# One-click horizontal scaling with automatic Caddy load balancing
kizuna scale web 3

# Instant rollback to previous stable release
kizuna rollback web
```

### 5. Launch the Real-Time TUI Dashboard & Resource Manager
```bash
# Launch interactive Charm Bubble Tea dashboard (Resource Manager, Nodes, Workloads, Logs)
kizuna dashboard

# Or inspect live system and node telemetry in terminal
kizuna status
```

---

## ⚙️ Configuration (`kizuna.yaml`)

### Advanced Web Application with Presets & LAN / Homelab HTTPS
```yaml
version: "1"
name: "fullstack-workspace"
target: "home-server"
theme: "tokyonight"

services:
  web:
    type: "docker"
    dockerfile: "Dockerfile"
    ports:
      - "3000:3000"
    replicas: 2
    ingress:
      provider: "caddy"
      domain: "192.168.1.100"   # LAN IP automatically enables 'tls internal' (Local Root CA)
      # Or for public domains in homelab without public ports:
      # domain: "nas.mydomain.com"
      # tls: "cloudflare"       # Uses Cloudflare DNS-01 ACME challenge for valid Let's Encrypt certs!
      websocket: true          # Auto-configures WebSocket reverse proxy
      sse: true                # Real-time SSE & AI streaming (flush_interval -1)
      cors: true               # Auto-injects CORS headers
      lb_policy: "round_robin" # Load balancing across replicas

  # 2. Go / Node Backend (Direct Dockerfile)
  api:
    root: "./apps/api"
    type: "docker"
    dockerfile: "./apps/api/Dockerfile"
    ports:
      - "8080:8080"
    env:
      PORT: "8080"
    ingress:
      provider: "caddy"
      domain: "api.example.com"
      auto_tls: true
      upstream_port: 8080

  # 3. Database (Docker Compose)
  db:
    type: "compose"
    compose_file: "./docker-compose.yml"
    backup:
      paths:
        - "./data/postgres"
      schedule: "0 3 * * *"
      storage:
        type: "s3"
        bucket: "my-backups"
        endpoint: "https://s3.us-west-004.backblazeb2.com"
```

### Single App Shorthand
```yaml
name: "my-app"
target: "home-server"
theme: "dracula"
type: "docker"
dockerfile: "Dockerfile"
ports:
  - "3000:3000"
ingress:
  provider: "caddy"
  domain: "mycoolsite.com"
  auto_tls: true
  upstream_port: 3000
```

---

## 🎨 Themes

Theme colors are fully customizable and not hardcoded! You can configure them:
1. In `kizuna.yaml` via `theme: "tokyonight"`
2. Via CLI flag: `kizuna dashboard --theme dracula`
3. Custom theme definition in `kizuna.yaml`:
   ```yaml
   theme: "my-custom"
   custom_theme:
     primary: "#E06C75"
     secondary: "#98C379"
     danger: "#BE5046"
     background: "#282C34"
     text: "#ABB2BF"
     box_border: "#61AFEF"
   ```

---

## 🌐 Localization (i18n)

Switch language automatically via system locale or force with `-l / --lang`:
```bash
# English (Default)
./kizuna --help

# Chinese (中文)
./kizuna -l zh --help

# Japanese (日本語)
./kizuna -l ja --help
```

---

## 📄 License
MIT License
