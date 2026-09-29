package presenter

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/telemetry"
)

var (
	// Global flags for AI and scripting
	JSONOutput bool
	NoColor    bool
)

// UI encapsulates Lipgloss styles based on current theme
type UI struct {
	Theme *entity.Theme

	PrimaryStyle   lipgloss.Style
	SecondaryStyle lipgloss.Style
	WarningStyle   lipgloss.Style
	DangerStyle    lipgloss.Style
	MutedStyle     lipgloss.Style
	BoldStyle      lipgloss.Style

	CardStyle lipgloss.Style
	PinStyle  lipgloss.Style
}

// NewUI initializes a UI styler with the specified or active theme
func NewUI(themeName string) *UI {
	t := entity.ResolveTheme(themeName, nil)

	primary := lipgloss.Color(t.Primary)
	secondary := lipgloss.Color(t.Secondary)
	warning := lipgloss.Color(t.Warning)
	danger := lipgloss.Color(t.Danger)
	muted := lipgloss.Color(t.Muted)

	return &UI{
		Theme: t,

		PrimaryStyle:   lipgloss.NewStyle().Foreground(primary),
		SecondaryStyle: lipgloss.NewStyle().Foreground(secondary),
		WarningStyle:   lipgloss.NewStyle().Foreground(warning),
		DangerStyle:    lipgloss.NewStyle().Foreground(danger),
		MutedStyle:     lipgloss.NewStyle().Foreground(muted),
		BoldStyle:      lipgloss.NewStyle().Bold(true),

		CardStyle: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(primary).
			Padding(1, 2),

		PinStyle: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(primary).
			Padding(0, 2),
	}
}

// Banner returns the stylized Kizuna brand header
func (u *UI) Banner() string {
	if NoColor || os.Getenv("NO_COLOR") != "" {
		return "=== 絆 KIZUNA • Zero-Trust Mesh Deployment & Ingress CLI ==="
	}

	logoArt := `  ██╗  ██╗██╗███████╗██╗   ██╗███╗   ██╗ █████╗ 
  ██║ ██╔╝██║╚══███╔╝██║   ██║████╗  ██║██╔══██╗
  █████╔╝ ██║  ███╔╝ ██║   ██║██╔██╗ ██║███████║
  ██╔═██╗ ██║ ███╔╝  ██║   ██║██║╚██╗██║██╔══██║
  ██║  ██╗██║███████╗╚██████╔╝██║ ╚████║██║  ██║
  ╚═╝  ╚═╝╚═╝╚══════╝ ╚═════╝ ╚═╝  ╚═══╝╚═╝  ╚═╝`

	styledLogo := u.PrimaryStyle.Bold(true).Render(logoArt)

	badge := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(lipgloss.Color(u.Theme.Primary)).
		Padding(0, 1).
		Render("絆 KIZUNA")

	tagline := u.SecondaryStyle.Bold(true).Render("Zero-Trust Mesh Deployment & Website Ingress")

	return "\n" + styledLogo + "\n\n  " + badge + " " + tagline + "\n"
}

// Badge returns a pill badge
func (u *UI) Badge(text, bgColor, fgColor string) string {
	if NoColor {
		return "[" + text + "]"
	}
	return lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(fgColor)).
		Background(lipgloss.Color(bgColor)).
		Padding(0, 1).
		Render(text)
}

// StatusPill returns an online/offline indicator
func (u *UI) StatusPill(isOnline bool) string {
	if NoColor {
		if isOnline {
			return "[Online]"
		}
		return "[Offline]"
	}
	if isOnline {
		return lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color(u.Theme.Secondary)).
			Padding(0, 1).
			Render("● Online")
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(lipgloss.Color(u.Theme.Muted)).
		Padding(0, 1).
		Render("○ Offline")
}

// EnvBadge returns a badge for active environment
func (u *UI) EnvBadge(env string) string {
	if env == "" {
		env = "production"
	}
	bg := u.Theme.Primary
	switch strings.ToLower(env) {
	case "development", "dev":
		bg = "#F9E2AF" // yellow
	case "staging", "stage":
		bg = "#89B4FA" // blue
	case "test":
		bg = "#A6E3A1" // green
	case "production", "prod":
		bg = "#CBA6F7" // mauve
	}
	return u.Badge(strings.ToUpper(env), bg, "#11111B")
}

// RenderServerStart outputs the server card with mesh address and PIN
func (u *UI) RenderServerStart(meshAddr, pin string) string {
	if NoColor {
		return fmt.Sprintf("=== Kizuna Server Started ===\nMesh Address: %s\nPairing PIN: %s (valid for 10m)\n", meshAddr, pin)
	}

	header := u.PrimaryStyle.Bold(true).Render("🌐 KIZUNA SERVER DAEMON") + "  " + u.Badge("ACTIVE", u.Theme.Secondary, "#000")
	addrTitle := u.MutedStyle.Render("Tailcat Mesh Address:")
	addrBox := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(lipgloss.Color("#181825")).
		Padding(0, 1).
		Render(meshAddr)

	pinTitle := u.MutedStyle.Render("Pairing PIN (valid for 10 minutes):")
	pinBox := u.PinStyle.Render(" " + pin + " ")

	content := fmt.Sprintf("%s\n\n%s\n%s\n\n%s\n%s", header, addrTitle, addrBox, pinTitle, pinBox)
	return u.CardStyle.Render(content)
}

// RenderNodeTable formats paired nodes into a Lipgloss table
func (u *UI) RenderNodeTable(nodes []*entity.Node) string {
	if len(nodes) == 0 {
		return u.MutedStyle.Render("No paired nodes found. Pair one with: kizuna node add <mesh-addr> --pin <pin>")
	}

	headers := []string{"NAME", "STATUS", "TAGS", "HOST / IP", "OS / ARCH", "LAST SEEN"}
	rows := [][]string{}

	for _, n := range nodes {
		osArch := fmt.Sprintf("%s/%s", n.OS, n.Arch)
		if n.OS == "" {
			osArch = "unknown"
		}
		lastSeen := "Just now"
		if !n.LastSeen.IsZero() {
			lastSeen = n.LastSeen.Format("15:04:05")
		}

		tagsStr := u.MutedStyle.Render("-")
		if len(n.Tags) > 0 {
			tagsStr = strings.Join(n.Tags, ", ")
		}

		hostStr := u.MutedStyle.Render("-")
		if n.Host != "" {
			hostStr = n.Host
		} else if n.IP != "" {
			hostStr = n.IP
		}

		rows = append(rows, []string{
			u.BoldStyle.Render(n.Name),
			u.StatusPill(n.IsOnline),
			tagsStr,
			hostStr,
			osArch,
			u.MutedStyle.Render(lastSeen),
		})
	}

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color(u.Theme.Primary))).
		Headers(headers...).
		Rows(rows...)

	return t.Render()
}

// RenderServicesTable formats running services into a Lipgloss table
func (u *UI) RenderServicesTable(services []*entity.Service) string {
	if len(services) == 0 {
		return u.MutedStyle.Render("No deployed services found on node.")
	}

	headers := []string{"SERVICE", "TYPE", "STATUS", "PORTS", "DOMAIN"}
	rows := [][]string{}

	for _, s := range services {
		domain := "-"
		if s.Ingress != nil && s.Ingress.Domain != "" {
			domain = s.Ingress.Domain
		}
		ports := strings.Join(s.Ports, ", ")
		if ports == "" {
			ports = "-"
		}

		var statusBadge string
		switch s.State {
		case entity.StateRunning:
			statusBadge = u.Badge("RUNNING", u.Theme.Secondary, "#000")
		case entity.StateFailed:
			statusBadge = u.Badge("FAILED", u.Theme.Danger, "#FFF")
		default:
			statusBadge = u.Badge(string(s.State), u.Theme.Muted, "#FFF")
		}

		rows = append(rows, []string{
			u.BoldStyle.Render(s.Name),
			string(s.Type),
			statusBadge,
			ports,
			u.PrimaryStyle.Render(domain),
		})
	}

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color(u.Theme.Primary))).
		Headers(headers...).
		Rows(rows...)

	return t.Render()
}

// RenderSystemCard renders a comprehensive system resource manager card
func (u *UI) RenderSystemCard(m *telemetry.Metrics) string {
	if m == nil {
		return ""
	}

	cpuBar := renderBar(m.CPUUsage, 18)
	memBar := renderBar(m.MemoryUsage, 18)
	swapBar := renderBar(m.SwapUsage, 18)

	usedMem := formatBytes(m.UsedMemory)
	totMem := formatBytes(m.TotalMemory)
	availMem := formatBytes(m.AvailableMemory)
	usedSwap := formatBytes(m.UsedSwap)
	totSwap := formatBytes(m.TotalSwap)

	title := u.PrimaryStyle.Bold(true).Render("⚡ SYSTEM RESOURCE MONITOR & TELEMETRY")
	modelStr := m.CPUModel
	if len(modelStr) > 36 {
		modelStr = modelStr[:36] + "..."
	}
	sysInfo := fmt.Sprintf("Host: %-16s OS: %s (%s)   Kernel: %s   Uptime: %s",
		u.BoldStyle.Render(m.Hostname), m.OS, m.Arch, m.KernelVer, formatUptime(m.Uptime))

	cpuLine := fmt.Sprintf("CPU  [%s] %5.1f%% (%d cores, load: %.2f, %.2f, %.2f) %s",
		cpuBar, m.CPUUsage, m.CPUCores, m.Load1, m.Load5, m.Load15, u.MutedStyle.Render(modelStr))
	memLine := fmt.Sprintf("RAM  [%s] %5.1f%% (%s / %s, free: %s)",
		memBar, m.MemoryUsage, usedMem, totMem, availMem)
	swapLine := fmt.Sprintf("SWAP [%s] %5.1f%% (%s / %s)",
		swapBar, m.SwapUsage, usedSwap, totSwap)

	// Disks breakdown
	var diskLines []string
	if len(m.Partitions) > 0 {
		for _, p := range m.Partitions {
			pBar := renderBar(p.UsedPercent, 14)
			diskLines = append(diskLines, fmt.Sprintf("  • %-18s [%s] %5.1f%%  %s / %s (%s)",
				p.Mountpoint, pBar, p.UsedPercent, formatBytes(p.Used), formatBytes(p.Total), p.Fstype))
		}
	} else {
		diskBar := renderBar(m.DiskUsage, 18)
		diskLines = append(diskLines, fmt.Sprintf("DISK [%s] %5.1f%% (%s / %s)",
			diskBar, m.DiskUsage, formatBytes(m.UsedDisk), formatBytes(m.TotalDisk)))
	}

	// Network I/O
	netLine := fmt.Sprintf("NET  Total RX: %-12s Total TX: %-12s Rate: ↓ %s/s  ↑ %s/s",
		formatBytes(m.BytesRecv), formatBytes(m.BytesSent), formatBytes(m.RxRate), formatBytes(m.TxRate))

	// Top Processes
	var procLines []string
	if len(m.TopProcesses) > 0 {
		procHeader := u.MutedStyle.Render("  PID     COMMAND          CPU%     MEM%    RSS MEMORY")
		procLines = append(procLines, procHeader)
		for _, proc := range m.TopProcesses {
			procLines = append(procLines, fmt.Sprintf("  %-7d %-16s %5.1f%%   %5.1f%%   %s",
				proc.PID, truncate(proc.Name, 15), proc.CPUPercent, proc.MemoryPercent, formatBytes(proc.MemoryBytes)))
		}
	}

	var sb strings.Builder
	sb.WriteString(title + "\n")
	sb.WriteString(sysInfo + "\n\n")
	sb.WriteString(cpuLine + "\n")
	sb.WriteString(memLine + "\n")
	if m.TotalSwap > 0 {
		sb.WriteString(swapLine + "\n")
	}
	sb.WriteString(netLine + "\n\n")

	sb.WriteString(u.SecondaryStyle.Bold(true).Render("💾 STORAGE & PARTITIONS") + "\n")
	for _, dl := range diskLines {
		sb.WriteString(dl + "\n")
	}

	if len(procLines) > 0 {
		sb.WriteString("\n" + u.SecondaryStyle.Bold(true).Render("📊 TOP PROCESSES (TASK MANAGER)") + "\n")
		for _, pl := range procLines {
			sb.WriteString(pl + "\n")
		}
	}

	return u.CardStyle.Render(sb.String())
}

// PrintJSON marshals data to clean JSON for AI and automation scripts
func PrintJSON(w io.Writer, data any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

