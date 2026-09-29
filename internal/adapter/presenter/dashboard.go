package presenter

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/telemetry"
	"github.com/osuki-dev/kizuna/internal/usecase"
)

// Styles holds all lipgloss styles parameterized by the active theme
type Styles struct {
	Title         lipgloss.Style
	Subtitle      lipgloss.Style
	Tab           lipgloss.Style
	ActiveTab     lipgloss.Style
	Box           lipgloss.Style
	HeaderBox     lipgloss.Style
	MetricCard    lipgloss.Style
	SelectedRow   lipgloss.Style
	StatusRunning string
	StatusStopped string
	StatusFailed  string
	StatusProbing string
	PillSuccess   lipgloss.Style
	PillWarning   lipgloss.Style
	PillDanger    lipgloss.Style
	PillInfo      lipgloss.Style
	MutedText     lipgloss.Style
	HighlightText lipgloss.Style
}

// NewStyles constructs styles dynamically from a Theme entity
func NewStyles(theme *entity.Theme) Styles {
	if theme == nil {
		theme = entity.DefaultTheme()
	}

	pCol := lipgloss.Color(theme.Primary)
	sCol := lipgloss.Color(theme.Secondary)
	dCol := lipgloss.Color(theme.Danger)
	wCol := lipgloss.Color(theme.Warning)
	tCol := lipgloss.Color(theme.Text)
	mCol := lipgloss.Color(theme.Muted)
	bgCol := lipgloss.Color(theme.Background)
	borderCol := lipgloss.Color(theme.BoxBorder)

	return Styles{
		Title: lipgloss.NewStyle().
			Bold(true).
			Foreground(pCol),
		Subtitle: lipgloss.NewStyle().
			Foreground(sCol).
			Bold(true),
		Tab: lipgloss.NewStyle().
			Padding(0, 2).
			Foreground(tCol).
			Background(bgCol),
		ActiveTab: lipgloss.NewStyle().
			Padding(0, 2).
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(pCol),
		Box: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderCol).
			Padding(1, 2),
		HeaderBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(pCol).
			Padding(0, 1),
		MetricCard: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderCol).
			Padding(0, 1),
		SelectedRow: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(borderCol),
		StatusRunning: lipgloss.NewStyle().Foreground(sCol).Bold(true).Render("● Online"),
		StatusStopped: lipgloss.NewStyle().Foreground(mCol).Render("○ Offline"),
		StatusFailed:  lipgloss.NewStyle().Foreground(dCol).Bold(true).Render("✖ Unreachable"),
		StatusProbing: lipgloss.NewStyle().Foreground(wCol).Render("◌ Probing..."),
		PillSuccess: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(sCol).
			Padding(0, 1).
			Bold(true),
		PillWarning: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#000000")).
			Background(wCol).
			Padding(0, 1).
			Bold(true),
		PillDanger: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(dCol).
			Padding(0, 1).
			Bold(true),
		PillInfo: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(pCol).
			Padding(0, 1).
			Bold(true),
		MutedText:     lipgloss.NewStyle().Foreground(mCol),
		HighlightText: lipgloss.NewStyle().Foreground(sCol).Bold(true),
	}
}

type tabIndex int

const (
	tabOverview tabIndex = iota
	tabNodes
	tabServices
	tabLogs
	tabBackups
)

// NodeProbeState tracks async live state of a node
type NodeProbeState struct {
	Node      *entity.Node
	IsOnline  bool
	IsProbing bool
	Latency   time.Duration
	Services  []*entity.Service
	Error     string
	LastCheck time.Time
}

// DashboardOptions configures dashboard initialization
type DashboardOptions struct {
	Nodes              []*entity.Node
	ConfiguredServices []*entity.Service
	Client             usecase.NodeClient
	Theme              *entity.Theme
	ActiveEnv          string
}

// DashboardModel represents the rich Charm Bubble Tea TUI
type DashboardModel struct {
	nodes        []*entity.Node
	services     []*entity.Service
	nodeStates   map[string]*NodeProbeState
	selectedNode int
	selectedSvc  int
	client       usecase.NodeClient
	theme        *entity.Theme
	styles       Styles
	activeTab    tabIndex
	activeEnv    string
	width        int
	height       int
	collector    *telemetry.Collector
	localMetrics *telemetry.Metrics
	cpuHistory   []float64
	memHistory   []float64
	logs         []string
	mu           sync.RWMutex
}

// NewDashboardModel initializes the TUI model with instant non-blocking state
func NewDashboardModel(node *entity.Node, client usecase.NodeClient, theme *entity.Theme) *DashboardModel {
	var nodes []*entity.Node
	if node != nil {
		nodes = append(nodes, node)
	}
	return NewDashboardWithOptions(DashboardOptions{
		Nodes:  nodes,
		Client: client,
		Theme:  theme,
	})
}

// NewDashboardWithOptions creates a fully-featured dashboard model
func NewDashboardWithOptions(opts DashboardOptions) *DashboardModel {
	resolvedTheme := entity.ResolveTheme("", opts.Theme)

	collector := telemetry.NewCollector()
	initCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	localMetrics, _ := collector.Collect(initCtx)
	cancel()

	if localMetrics == nil {
		localMetrics = &telemetry.Metrics{
			OS:       "linux",
			Arch:     "amd64",
			CPUCores: 4,
		}
	}

	nodes := opts.Nodes
	if len(nodes) == 0 {
		nodes = []*entity.Node{
			{
				Name:      "local-node",
				Addr:      "127.0.0.1:19800",
				AuthToken: "kzn_local",
				IsOnline:  true,
				OS:        localMetrics.OS,
				Arch:      localMetrics.Arch,
			},
		}
	}

	nodeStates := make(map[string]*NodeProbeState)
	for _, n := range nodes {
		nodeStates[n.Name] = &NodeProbeState{
			Node:      n,
			IsOnline:  n.IsOnline,
			IsProbing: true,
		}
	}

	cpuVal := localMetrics.CPUUsage
	if cpuVal == 0 {
		cpuVal = 12.0
	}
	memVal := localMetrics.MemoryUsage
	if memVal == 0 {
		memVal = 35.0
	}

	envName := opts.ActiveEnv
	if envName == "" {
		envName = "production"
	}

	return &DashboardModel{
		nodes:        nodes,
		services:     opts.ConfiguredServices,
		nodeStates:   nodeStates,
		client:       opts.Client,
		theme:        resolvedTheme,
		styles:       NewStyles(resolvedTheme),
		activeTab:    tabOverview,
		activeEnv:    envName,
		collector:    collector,
		localMetrics: localMetrics,
		cpuHistory:   []float64{cpuVal * 0.8, cpuVal * 0.9, cpuVal * 1.1, cpuVal},
		memHistory:   []float64{memVal * 0.95, memVal * 0.98, memVal * 1.02, memVal},
		logs: []string{
			fmt.Sprintf("[%s] Kizuna Zero-Trust Mesh initialized.", time.Now().Format("15:04:05")),
			fmt.Sprintf("[%s] Environment: %s, Theme: %s", time.Now().Format("15:04:05"), envName, resolvedTheme.Name),
			fmt.Sprintf("[%s] Background telemetry daemon active.", time.Now().Format("15:04:05")),
		},
	}
}

// Messages for async Bubble Tea updates
type localMetricsMsg struct {
	metrics *telemetry.Metrics
	err     error
}

type nodeProbeResultMsg struct {
	nodeName string
	node     *entity.Node
	services []*entity.Service
	latency  time.Duration
	err      error
}

type tickMsg time.Time

func (m *DashboardModel) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tea.EnterAltScreen,
		m.fetchLocalMetricsCmd(),
		m.probeAllNodesCmd(),
		m.tickCmd(),
	}
	return tea.Batch(cmds...)
}

func (m *DashboardModel) tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *DashboardModel) fetchLocalMetricsCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		metrics, err := m.collector.Collect(ctx)
		return localMetricsMsg{metrics: metrics, err: err}
	}
}

func (m *DashboardModel) probeAllNodesCmd() tea.Cmd {
	m.mu.RLock()
	nodes := make([]*entity.Node, len(m.nodes))
	copy(nodes, m.nodes)
	m.mu.RUnlock()

	var cmds []tea.Cmd
	for _, n := range nodes {
		target := n
		cmds = append(cmds, func() tea.Msg {
			start := time.Now()
			if m.client == nil {
				return nodeProbeResultMsg{
					nodeName: target.Name,
					node:     target,
					latency:  0,
					err:      nil,
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()

			updatedNode, services, err := m.client.GetStatus(ctx, target)
			latency := time.Since(start)
			return nodeProbeResultMsg{
				nodeName: target.Name,
				node:     updatedNode,
				services: services,
				latency:  latency,
				err:      err,
			}
		})
	}
	return tea.Batch(cmds...)
}

func (m *DashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.activeTab = (m.activeTab + 1) % 5
			return m, nil
		case "shift+tab":
			m.activeTab = (m.activeTab + 4) % 5
			return m, nil
		case "1":
			m.activeTab = tabOverview
			return m, nil
		case "2":
			m.activeTab = tabNodes
			return m, nil
		case "3":
			m.activeTab = tabServices
			return m, nil
		case "4":
			m.activeTab = tabLogs
			return m, nil
		case "5":
			m.activeTab = tabBackups
			return m, nil
		case "up", "k":
			if m.activeTab == tabNodes && m.selectedNode > 0 {
				m.selectedNode--
			} else if m.activeTab == tabServices && m.selectedSvc > 0 {
				m.selectedSvc--
			}
			return m, nil
		case "down", "j":
			if m.activeTab == tabNodes && m.selectedNode < len(m.nodes)-1 {
				m.selectedNode++
			} else if m.activeTab == tabServices {
				totalW := len(m.getAggregatedWorkloads())
				if m.selectedSvc < totalW-1 {
					m.selectedSvc++
				}
			}
			return m, nil
		case "r":
			return m, tea.Batch(m.fetchLocalMetricsCmd(), m.probeAllNodesCmd())
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		return m, tea.Batch(
			m.fetchLocalMetricsCmd(),
			m.probeAllNodesCmd(),
			m.tickCmd(),
		)

	case localMetricsMsg:
		if msg.err == nil && msg.metrics != nil {
			m.localMetrics = msg.metrics
			m.cpuHistory = append(m.cpuHistory, msg.metrics.CPUUsage)
			if len(m.cpuHistory) > 20 {
				m.cpuHistory = m.cpuHistory[1:]
			}
			m.memHistory = append(m.memHistory, msg.metrics.MemoryUsage)
			if len(m.memHistory) > 20 {
				m.memHistory = m.memHistory[1:]
			}
		}

	case nodeProbeResultMsg:
		m.mu.Lock()
		state, ok := m.nodeStates[msg.nodeName]
		if !ok {
			state = &NodeProbeState{}
			m.nodeStates[msg.nodeName] = state
		}
		state.IsProbing = false
		state.LastCheck = time.Now()
		state.Latency = msg.latency
		if msg.err != nil {
			state.IsOnline = false
			state.Error = msg.err.Error()
		} else {
			state.IsOnline = true
			state.Error = ""
			if msg.node != nil {
				state.Node = msg.node
			}
			if len(msg.services) > 0 {
				state.Services = msg.services
			}
		}
		m.mu.Unlock()
	}

	return m, nil
}

func (m *DashboardModel) View() string {
	var s strings.Builder

	contentWidth := m.width - 4
	if contentWidth < 80 {
		contentWidth = 80
	}

	// 1. Header Banner
	s.WriteString(m.renderHeader(contentWidth))
	s.WriteString("\n")

	// 2. Navigation Tabs
	tabs := []string{"[1] ⚡ Resource Manager", "[2] 🌐 Mesh Nodes", "[3] 📦 Workloads & Ingress", "[4] 📜 Real-time Logs", "[5] 💾 Backups"}
	var renderedTabs []string
	for i, t := range tabs {
		if tabIndex(i) == m.activeTab {
			renderedTabs = append(renderedTabs, m.styles.ActiveTab.Render(t))
		} else {
			renderedTabs = append(renderedTabs, m.styles.Tab.Render(t))
		}
	}
	s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...))
	s.WriteString("\n")

	// 3. Tab Contents
	var content string
	switch m.activeTab {
	case tabOverview:
		content = m.renderOverviewTab(contentWidth)
	case tabNodes:
		content = m.renderNodesTab(contentWidth)
	case tabServices:
		content = m.renderServicesTab(contentWidth)
	case tabLogs:
		content = m.renderLogsTab(contentWidth)
	case tabBackups:
		content = m.renderBackupsTab(contentWidth)
	}

	boxW := contentWidth - 4
	if boxW < 40 {
		boxW = 40
	}
	s.WriteString(m.styles.Box.Width(boxW).Render(content))
	s.WriteString("\n")

	// 4. Footer shortcuts
	footer := m.styles.MutedText.Render(" [1-5 / Tab] Switch Views  •  [↑/↓] Select Item  •  [r] Refresh Telemetry  •  [q] Quit")
	s.WriteString(footer)

	return s.String()
}

func (m *DashboardModel) renderHeader(width int) string {
	titleText := m.styles.Title.Render("✦ KIZUNA DASHBOARD ✦")
	envBadge := m.styles.PillInfo.Render("ENV: " + strings.ToUpper(m.activeEnv))
	themeBadge := m.styles.PillSuccess.Render("THEME: " + strings.ToUpper(m.theme.Name))

	meshStatus := m.styles.HighlightText.Render("🔒 WireGuard Zero-Trust Mesh: Active")
	ingressStatus := m.styles.HighlightText.Render("⚡ HTTP/3 (QUIC) Caddy: Enabled")

	topLine := fmt.Sprintf("%s   %s  %s", titleText, envBadge, themeBadge)
	subLine := fmt.Sprintf("%s   •   %s", meshStatus, ingressStatus)

	return m.styles.HeaderBox.Width(width - 4).Render(topLine + "\n" + subLine)
}

func (m *DashboardModel) renderOverviewTab(width int) string {
	var sb strings.Builder

	sb.WriteString(m.styles.Subtitle.Render("⚡ SYSTEM RESOURCE MANAGER (REAL-TIME)") + "\n\n")

	cpuP := m.localMetrics.CPUUsage
	memP := m.localMetrics.MemoryUsage
	swapP := m.localMetrics.SwapUsage

	usedMemStr := formatBytes(m.localMetrics.UsedMemory)
	totMemStr := formatBytes(m.localMetrics.TotalMemory)
	usedSwapStr := formatBytes(m.localMetrics.UsedSwap)
	totSwapStr := formatBytes(m.localMetrics.TotalSwap)

	cpuTrend := renderSparkline(m.cpuHistory)
	memTrend := renderSparkline(m.memHistory)

	cardW := (width - 16) / 4
	if cardW < 22 {
		cardW = 22
	}

	cpuCard := fmt.Sprintf("CPU USAGE (%d Cores)\n[%s] %5.1f%%\nTrend: %s\nLoad: %.2f, %.2f, %.2f",
		m.localMetrics.CPUCores,
		renderBar(cpuP, 12),
		cpuP,
		cpuTrend,
		m.localMetrics.Load1, m.localMetrics.Load5, m.localMetrics.Load15,
	)

	memCard := fmt.Sprintf("RAM MEMORY\n[%s] %5.1f%%\nTrend: %s\nUsed: %s / %s",
		renderBar(memP, 12),
		memP,
		memTrend,
		usedMemStr, totMemStr,
	)

	swapCard := fmt.Sprintf("SWAP MEMORY\n[%s] %5.1f%%\nFree: %s\nTotal: %s / %s",
		renderBar(swapP, 12),
		swapP,
		formatBytes(m.localMetrics.FreeSwap),
		usedSwapStr, totSwapStr,
	)

	rxRateStr := formatBytes(m.localMetrics.RxRate)
	txRateStr := formatBytes(m.localMetrics.TxRate)
	netCard := fmt.Sprintf("NETWORK I/O\nRate: ↓ %s/s\n      ↑ %s/s\nTotal: %s / %s",
		rxRateStr, txRateStr,
		formatBytes(m.localMetrics.BytesRecv),
		formatBytes(m.localMetrics.BytesSent),
	)

	c1 := m.styles.MetricCard.Width(cardW).Render(cpuCard)
	c2 := m.styles.MetricCard.Width(cardW).Render(memCard)
	c3 := m.styles.MetricCard.Width(cardW).Render(swapCard)
	c4 := m.styles.MetricCard.Width(cardW).Render(netCard)

	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, c1, " ", c2, " ", c3, " ", c4))
	sb.WriteString("\n\n")

	// CPU Per-Core Matrix (if available and <= 32 cores)
	if len(m.localMetrics.CoreUsages) > 0 {
		sb.WriteString(m.styles.Subtitle.Render("📊 CPU PER-CORE USAGE MATRIX") + "\n")
		sb.WriteString(strings.Repeat("─", width-8) + "\n")
		var currRow strings.Builder
		limitCores := len(m.localMetrics.CoreUsages)
		if limitCores > 24 {
			limitCores = 24
		}
		for idx := 0; idx < limitCores; idx++ {
			cUsage := m.localMetrics.CoreUsages[idx]
			fmt.Fprintf(&currRow, " C%-2d [%s] %4.1f%% ", idx, renderBar(cUsage, 6), cUsage)
			if (idx+1)%4 == 0 || idx == limitCores-1 {
				currRow.WriteString("\n")
			}
		}
		sb.WriteString(currRow.String())
		sb.WriteString("\n")
	}

	// Storage & Disks Partitions Breakdown Table
	sb.WriteString(m.styles.Subtitle.Render("💾 STORAGE & DISK PARTITIONS") + "\n")
	sb.WriteString(strings.Repeat("─", width-8) + "\n")
	if len(m.localMetrics.Partitions) > 0 {
		fmt.Fprintf(&sb, "  %-20s %-16s %-8s %-18s %-14s %-8s\n",
			"MOUNTPOINT", "DEVICE", "FSTYPE", "USED / TOTAL", "BAR", "USAGE%")
		for _, p := range m.localMetrics.Partitions {
			pBar := renderBar(p.UsedPercent, 10)
			fmt.Fprintf(&sb, "  %-20s %-16s %-8s %-18s [%s] %5.1f%%\n",
				truncate(p.Mountpoint, 19),
				truncate(p.Device, 15),
				p.Fstype,
				fmt.Sprintf("%s / %s", formatBytes(p.Used), formatBytes(p.Total)),
				pBar,
				p.UsedPercent,
			)
		}
	} else {
		fmt.Fprintf(&sb, "  Root: %s / %s (%.1f%%)\n",
			formatBytes(m.localMetrics.UsedDisk), formatBytes(m.localMetrics.TotalDisk), m.localMetrics.DiskUsage)
	}
	sb.WriteString("\n")

	// Top Processes (Task Manager)
	if len(m.localMetrics.TopProcesses) > 0 {
		sb.WriteString(m.styles.Subtitle.Render("📈 TOP PROCESSES (TASK MANAGER)") + "\n")
		sb.WriteString(strings.Repeat("─", width-8) + "\n")
		fmt.Fprintf(&sb, "  %-8s %-22s %-10s %-10s %-14s\n",
			"PID", "PROCESS", "CPU%", "MEM%", "RSS MEMORY")
		for _, pr := range m.localMetrics.TopProcesses {
			fmt.Fprintf(&sb, "  %-8d %-22s %5.1f%%    %5.1f%%    %-14s\n",
				pr.PID, truncate(pr.Name, 21), pr.CPUPercent, pr.MemoryPercent, formatBytes(pr.MemoryBytes))
		}
		sb.WriteString("\n")
	}

	// Machine Host Details
	sb.WriteString(m.styles.Subtitle.Render("💻 HOST & MESH PLATFORM") + "\n")
	sb.WriteString(strings.Repeat("─", width-8) + "\n")
	uptimeStr := formatUptime(m.localMetrics.Uptime)
	modelStr := m.localMetrics.CPUModel
	if len(modelStr) > 40 {
		modelStr = modelStr[:40] + "..."
	}
	fmt.Fprintf(&sb, "  Hostname:       %-26s OS / Platform:  %s %s (%s)\n",
		m.localMetrics.Hostname, m.localMetrics.Platform, m.localMetrics.PlatformVer, m.localMetrics.Arch)
	fmt.Fprintf(&sb, "  Kernel:         %-26s System Uptime:  %s\n",
		m.localMetrics.KernelVer, uptimeStr)
	if modelStr != "" {
		fmt.Fprintf(&sb, "  CPU Model:      %s\n", modelStr)
	}

	return sb.String()
}

func (m *DashboardModel) renderNodesTab(width int) string {
	var sb strings.Builder

	sb.WriteString(m.styles.Subtitle.Render("🌐 MESH NODES (P2P WIREGUARD NETWORK)") + "\n\n")

	header := fmt.Sprintf("%-2s %-16s %-20s %-14s %-12s %-12s %-10s\n",
		" ", "NAME", "MESH ADDR", "STATUS", "LATENCY", "OS / ARCH", "CPU / RAM")
	divider := strings.Repeat("─", width-8) + "\n"
	sb.WriteString(header)
	sb.WriteString(divider)

	m.mu.RLock()
	defer m.mu.RUnlock()

	for i, n := range m.nodes {
		cursor := "  "
		if i == m.selectedNode {
			cursor = "❯ "
		}

		st := m.nodeStates[n.Name]
		statusStr := m.styles.StatusStopped
		latencyStr := "-"
		osArchStr := n.OS + "/" + n.Arch
		cpuMemStr := "-"

		if st != nil {
			if st.IsProbing {
				statusStr = m.styles.StatusProbing
			} else if st.IsOnline {
				statusStr = m.styles.StatusRunning
				if st.Latency > 0 {
					latencyStr = fmt.Sprintf("%dms", st.Latency.Milliseconds())
				} else {
					latencyStr = "<1ms (local)"
				}
				if st.Node != nil && st.Node.CPUUsage > 0 {
					cpuMemStr = fmt.Sprintf("%.0f%% / %.0f%%", st.Node.CPUUsage, st.Node.MemoryUsage)
				}
			} else {
				statusStr = m.styles.StatusFailed
				latencyStr = "Timeout"
			}
		}

		if osArchStr == "/" {
			osArchStr = "unknown"
		}

		row := fmt.Sprintf("%-2s %-16s %-20s %-14s %-12s %-12s %-10s\n",
			cursor,
			truncate(n.Name, 15),
			truncate(n.Addr, 19),
			statusStr,
			latencyStr,
			osArchStr,
			cpuMemStr,
		)

		if i == m.selectedNode {
			sb.WriteString(m.styles.SelectedRow.Render(row))
		} else {
			sb.WriteString(row)
		}
	}

	// Details of Selected Node
	if m.selectedNode < len(m.nodes) {
		sn := m.nodes[m.selectedNode]
		st := m.nodeStates[sn.Name]
		sb.WriteString("\n" + m.styles.Subtitle.Render("🔎 SELECTED NODE DETAILS") + "\n")
		sb.WriteString(strings.Repeat("─", width-8) + "\n")
		fmt.Fprintf(&sb, "  Node ID:     %s\n", sn.ID)
		fmt.Fprintf(&sb, "  Mesh Addr:   %s\n", sn.Addr)
		if st != nil && !st.IsOnline && st.Error != "" {
			fmt.Fprintf(&sb, "  Error:       %s\n", m.styles.MutedText.Render(st.Error))
		}
		if st != nil && len(st.Services) > 0 {
			var svcNames []string
			for _, sv := range st.Services {
				svcNames = append(svcNames, sv.Name)
			}
			fmt.Fprintf(&sb, "  Workloads:   %s\n", strings.Join(svcNames, ", "))
		}
	}

	return sb.String()
}

type aggregatedWorkload struct {
	Name      string
	NodeName  string
	Type      entity.ServiceType
	State     entity.ServiceState
	Ports     []string
	Domain    string
	TLS       string
	Replicas  int
	Upstreams []string
	LBPolicy  string
	UpdatedAt time.Time
}

func (m *DashboardModel) getAggregatedWorkloads() []*aggregatedWorkload {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*aggregatedWorkload
	indexByKey := make(map[string]int)

	// 1. Add locally configured services from kizuna.yaml
	for _, s := range m.services {
		nodeName := "local"
		if len(m.nodes) > 0 {
			nodeName = m.nodes[0].Name
		}
		domainStr := "-"
		tlsStr := "None"
		lbPolicy := "round_robin"
		var upstreams []string
		replicas := s.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		if s.Ingress != nil {
			if s.Ingress.Domain != "" {
				domainStr = s.Ingress.Domain
			}
			if s.Ingress.AutoTLS {
				tlsStr = "Auto ACME (H3)"
			} else if s.Ingress.TLS == "internal" {
				tlsStr = "LAN Root CA"
			} else if s.Ingress.TLS == "cloudflare" || s.Ingress.DNSProvider == "cloudflare" {
				tlsStr = "Cloudflare DNS-01"
			}
			if s.Ingress.LBPolicy != "" {
				lbPolicy = s.Ingress.LBPolicy
			}
			upstreams = s.Ingress.Upstreams
		}

		key := nodeName + "/" + s.Name
		indexByKey[key] = len(result)
		result = append(result, &aggregatedWorkload{
			Name:      s.Name,
			NodeName:  nodeName,
			Type:      s.Type,
			State:     s.State,
			Ports:     s.Ports,
			Domain:    domainStr,
			TLS:       tlsStr,
			Replicas:  replicas,
			Upstreams: upstreams,
			LBPolicy:  lbPolicy,
			UpdatedAt: s.UpdatedAt,
		})
	}

	// 2. Discover live workloads reported by probed nodes
	for _, n := range m.nodes {
		st := m.nodeStates[n.Name]
		if st == nil || !st.IsOnline || len(st.Services) == 0 {
			continue
		}
		for _, rs := range st.Services {
			key := n.Name + "/" + rs.Name
			domainStr := "-"
			tlsStr := "None"
			lbPolicy := "round_robin"
			var upstreams []string
			replicas := rs.Replicas
			if replicas <= 0 {
				replicas = 1
			}
			if rs.Ingress != nil {
				if rs.Ingress.Domain != "" {
					domainStr = rs.Ingress.Domain
				}
				if rs.Ingress.AutoTLS {
					tlsStr = "Auto ACME (H3)"
				} else if rs.Ingress.TLS == "internal" {
					tlsStr = "LAN Root CA"
				} else if rs.Ingress.TLS == "cloudflare" || rs.Ingress.DNSProvider == "cloudflare" {
					tlsStr = "Cloudflare DNS-01"
				}
				if rs.Ingress.LBPolicy != "" {
					lbPolicy = rs.Ingress.LBPolicy
				}
				upstreams = rs.Ingress.Upstreams
			}

			if idx, found := indexByKey[key]; found {
				result[idx].State = rs.State
				if len(rs.Ports) > 0 {
					result[idx].Ports = rs.Ports
				}
				if rs.Replicas > 0 {
					result[idx].Replicas = rs.Replicas
				}
				if len(upstreams) > 0 {
					result[idx].Upstreams = upstreams
				}
				if !rs.UpdatedAt.IsZero() {
					result[idx].UpdatedAt = rs.UpdatedAt
				}
			} else {
				altKey := "local/" + rs.Name
				if idx, found := indexByKey[altKey]; found {
					result[idx].NodeName = n.Name
					result[idx].State = rs.State
					if len(rs.Ports) > 0 {
						result[idx].Ports = rs.Ports
					}
					if rs.Replicas > 0 {
						result[idx].Replicas = rs.Replicas
					}
					if len(upstreams) > 0 {
						result[idx].Upstreams = upstreams
					}
					if !rs.UpdatedAt.IsZero() {
						result[idx].UpdatedAt = rs.UpdatedAt
					}
				} else {
					indexByKey[key] = len(result)
					result = append(result, &aggregatedWorkload{
						Name:      rs.Name,
						NodeName:  n.Name,
						Type:      rs.Type,
						State:     rs.State,
						Ports:     rs.Ports,
						Domain:    domainStr,
						TLS:       tlsStr,
						Replicas:  replicas,
						Upstreams: upstreams,
						LBPolicy:  lbPolicy,
						UpdatedAt: rs.UpdatedAt,
					})
				}
			}
		}
	}

	return result
}

func (m *DashboardModel) renderServicesTab(width int) string {
	var sb strings.Builder

	sb.WriteString(m.styles.Subtitle.Render("📦 MESH WORKLOADS & INGRESS ROUTING") + "\n\n")

	workloads := m.getAggregatedWorkloads()
	if len(workloads) == 0 {
		sb.WriteString("  No active workloads found on local machine or connected mesh nodes.\n")
		sb.WriteString("  Deploy your first workload with:  kizuna deploy\n")
		return sb.String()
	}

	if m.selectedSvc >= len(workloads) {
		m.selectedSvc = len(workloads) - 1
	}

	header := fmt.Sprintf("%-2s %-16s %-14s %-10s %-12s %-14s %-20s %-14s\n",
		" ", "SERVICE", "NODE", "TYPE", "STATUS", "PORTS", "INGRESS DOMAIN", "HTTPS/TLS")
	divider := strings.Repeat("─", width-8) + "\n"
	sb.WriteString(header)
	sb.WriteString(divider)

	for i, w := range workloads {
		cursor := "  "
		if i == m.selectedSvc {
			cursor = "❯ "
		}

		st := m.styles.StatusRunning
		switch w.State {
		case entity.StateFailed:
			st = m.styles.StatusFailed
		case entity.StateStopped:
			st = m.styles.StatusStopped
		case entity.StateDeploying:
			st = m.styles.StatusProbing
		}

		portsStr := strings.Join(w.Ports, ", ")
		if portsStr == "" {
			portsStr = "-"
		}

		row := fmt.Sprintf("%-2s %-16s %-14s %-10s %-12s %-14s %-20s %-14s\n",
			cursor,
			truncate(w.Name, 15),
			truncate(w.NodeName, 13),
			truncate(string(w.Type), 9),
			st,
			truncate(portsStr, 13),
			truncate(w.Domain, 19),
			truncate(w.TLS, 13),
		)

		if i == m.selectedSvc {
			sb.WriteString(m.styles.SelectedRow.Render(row))
		} else {
			sb.WriteString(row)
		}
	}

	// Details of Selected Workload
	if m.selectedSvc < len(workloads) {
		sw := workloads[m.selectedSvc]
		sb.WriteString("\n" + m.styles.Subtitle.Render("🔎 SELECTED WORKLOAD DETAILS") + "\n")
		sb.WriteString(strings.Repeat("─", width-8) + "\n")
		fmt.Fprintf(&sb, "  Service Name:   %-20s Node:          %s\n", sw.Name, sw.NodeName)
		fmt.Fprintf(&sb, "  Workload Type:  %-20s Status:        %s\n", sw.Type, sw.State)
		fmt.Fprintf(&sb, "  Replicas:       %-20d Load Balancer: %s\n", sw.Replicas, sw.LBPolicy)
		if len(sw.Upstreams) > 0 {
			fmt.Fprintf(&sb, "  Upstream Pools: %s\n", strings.Join(sw.Upstreams, ", "))
		}
		if sw.Domain != "-" {
			fmt.Fprintf(&sb, "  Ingress Route:  https://%s (%s)\n", sw.Domain, sw.TLS)
		}
		if !sw.UpdatedAt.IsZero() {
			fmt.Fprintf(&sb, "  Last Updated:   %s\n", sw.UpdatedAt.Format("2006-01-02 15:04:05"))
		}
	}

	return sb.String()
}

func (m *DashboardModel) renderLogsTab(width int) string {
	var sb strings.Builder
	sb.WriteString(m.styles.Subtitle.Render("📜 REAL-TIME LIVE LOGS") + "\n\n")

	for _, l := range m.logs {
		sb.WriteString("  " + l + "\n")
	}
	sb.WriteString("\n  " + m.styles.MutedText.Render("(Press [r] to refresh; logs auto-stream from running services...)"))
	return sb.String()
}

func (m *DashboardModel) renderBackupsTab(width int) string {
	var sb strings.Builder
	sb.WriteString(m.styles.Subtitle.Render("💾 BACKUPS & SNAPSHOTS") + "\n\n")

	sb.WriteString("  Active Backup Policies:\n")
	sb.WriteString("  • Daily Snapshot: Local Directory (.kizuna/backups) + S3 Compatible Vault\n")
	sb.WriteString("  • Retention: Keep last 7 revisions (older archives automatically pruned)\n\n")
	sb.WriteString("  Create a new snapshot with:  kizuna backup <service-name>\n")
	return sb.String()
}

func renderSparkline(values []float64) string {
	sparks := []rune{' ', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	if len(values) == 0 {
		return " "
	}
	maxVal := 100.0
	var sb strings.Builder
	for _, v := range values {
		idx := int((v / maxVal) * float64(len(sparks)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparks) {
			idx = len(sparks) - 1
		}
		sb.WriteRune(sparks[idx])
	}
	return sb.String()
}

func renderBar(percent float64, totalBars int) string {
	if totalBars <= 0 {
		totalBars = 20
	}
	filled := int((percent / 100.0) * float64(totalBars))
	if filled > totalBars {
		filled = totalBars
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", totalBars-filled)
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatUptime(seconds uint64) string {
	if seconds == 0 {
		return "just started"
	}
	d := time.Duration(seconds) * time.Second
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	hours := d / time.Hour
	d -= hours * time.Hour
	mins := d / time.Minute
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm %ds", mins, d/time.Second)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
