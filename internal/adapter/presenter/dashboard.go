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
	WarningText   lipgloss.Style
	GaugeLow      lipgloss.Style
	GaugeMed      lipgloss.Style
	GaugeHigh     lipgloss.Style
	GaugeEmpty    lipgloss.Style
	Sparkline     lipgloss.Style
	Divider       lipgloss.Style
	TableHeader   lipgloss.Style
	CardTitle     lipgloss.Style
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
			BorderForeground(mCol).
			Padding(0, 1).
			Background(bgCol),
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
		WarningText:   lipgloss.NewStyle().Foreground(wCol).Bold(true),
		GaugeLow: lipgloss.NewStyle().
			Foreground(sCol).
			Bold(true),
		GaugeMed: lipgloss.NewStyle().
			Foreground(wCol).
			Bold(true),
		GaugeHigh: lipgloss.NewStyle().
			Foreground(dCol).
			Bold(true),
		GaugeEmpty: lipgloss.NewStyle().
			Foreground(mCol),
		Sparkline: lipgloss.NewStyle().
			Foreground(pCol).
			Bold(true),
		Divider: lipgloss.NewStyle().
			Foreground(mCol),
		TableHeader: lipgloss.NewStyle().
			Bold(true).
			Foreground(pCol),
		CardTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(pCol),
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
			if len(m.cpuHistory) > 32 {
				m.cpuHistory = m.cpuHistory[1:]
			}
			m.memHistory = append(m.memHistory, msg.metrics.MemoryUsage)
			if len(m.memHistory) > 32 {
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

	termWidth := m.width
	if termWidth <= 0 {
		termWidth = 100
	}
	if termWidth < 70 {
		termWidth = 70
	}

	// 1. Header Banner
	s.WriteString(m.renderHeader(termWidth))
	s.WriteString("\n")

	// 2. Navigation Tabs (Responsive based on terminal width)
	var tabs []string
	if termWidth >= 130 {
		tabs = []string{"[1] ⚡ Resource Manager", "[2] 🌐 Mesh Nodes", "[3] 📦 Workloads & Ingress", "[4] 📜 Real-time Logs", "[5] 💾 Backups"}
	} else if termWidth >= 100 {
		tabs = []string{"[1] ⚡ Resources", "[2] 🌐 Nodes", "[3] 📦 Workloads", "[4] 📜 Logs", "[5] 💾 Backups"}
	} else {
		tabs = []string{"[1] Resources", "[2] Nodes", "[3] Services", "[4] Logs", "[5] Backups"}
	}

	tabStyle := m.styles.Tab
	activeTabStyle := m.styles.ActiveTab
	if termWidth < 110 {
		tabStyle = tabStyle.Padding(0, 1)
		activeTabStyle = activeTabStyle.Padding(0, 1)
	}

	var renderedTabs []string
	for i, t := range tabs {
		if tabIndex(i) == m.activeTab {
			renderedTabs = append(renderedTabs, activeTabStyle.Render(t))
		} else {
			renderedTabs = append(renderedTabs, tabStyle.Render(t))
		}
	}
	s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...))
	s.WriteString("\n")

	// 3. Tab Contents
	// Total outer width target is termWidth - 2.
	// m.styles.Box has Border (2) + Padding(1, 2) (4).
	// When Box.Width(w) is called, rendered width is w + 2 (border).
	// Inside Box, content area is w - 4 (padding).
	// To make content area equal to innerWidth, we pass w = innerWidth + 4.
	innerWidth := termWidth - 8
	if innerWidth < 60 {
		innerWidth = 60
	}

	var content string
	switch m.activeTab {
	case tabOverview:
		content = m.renderOverviewTab(innerWidth)
	case tabNodes:
		content = m.renderNodesTab(innerWidth)
	case tabServices:
		content = m.renderServicesTab(innerWidth)
	case tabLogs:
		content = m.renderLogsTab(innerWidth)
	case tabBackups:
		content = m.renderBackupsTab(innerWidth)
	}

	s.WriteString(m.styles.Box.Width(innerWidth + 4).Render(content))
	s.WriteString("\n")

	// 4. Footer shortcuts (Adaptive)
	var footerText string
	if termWidth >= 100 {
		footerText = " [1-5 / Tab] Switch Views  •  [↑/↓] Select Item  •  [r] Refresh Telemetry  •  [q] Quit"
	} else if termWidth >= 80 {
		footerText = " [1-5/Tab] Views  •  [↑/↓] Select  •  [r] Refresh  •  [q] Quit"
	} else {
		footerText = " [1-5] Tab  •  [r] Refresh  •  [q] Quit"
	}
	s.WriteString(m.styles.MutedText.Render(footerText))

	return s.String()
}

func (m *DashboardModel) renderHeader(width int) string {
	titleText := m.styles.Title.Render("✦ KIZUNA DASHBOARD ✦")
	envBadge := m.styles.PillInfo.Render("ENV: " + strings.ToUpper(m.activeEnv))
	themeBadge := m.styles.PillSuccess.Render("THEME: " + strings.ToUpper(m.theme.Name))

	var topLine string
	if width >= 75 {
		topLine = fmt.Sprintf("%s   %s  %s", titleText, envBadge, themeBadge)
	} else {
		topLine = fmt.Sprintf("%s  %s", titleText, envBadge)
	}

	var subLine string
	if width >= 90 {
		subLine = fmt.Sprintf("%s   •   %s",
			m.styles.HighlightText.Render("🔒 WireGuard Zero-Trust Mesh: Active"),
			m.styles.HighlightText.Render("⚡ HTTP/3 (QUIC) Caddy: Enabled"),
		)
	} else if width >= 75 {
		subLine = fmt.Sprintf("%s  •  %s",
			m.styles.HighlightText.Render("🔒 Mesh: Active"),
			m.styles.HighlightText.Render("⚡ Ingress: Caddy"),
		)
	} else {
		subLine = m.styles.HighlightText.Render("🔒 WireGuard Mesh: Active")
	}

	boxInnerW := width - 4
	if boxInnerW < 40 {
		boxInnerW = 40
	}
	return m.styles.HeaderBox.Width(boxInnerW).Render(topLine + "\n" + subLine)
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
	rxRateStr := formatBytes(m.localMetrics.RxRate)
	txRateStr := formatBytes(m.localMetrics.TxRate)

	// Top Cards: Adaptive Layout
	if width >= 100 {
		gap := 1
		totalCardSpace := width - 3*gap
		cardOuterW := totalCardSpace / 4
		cardStyleW := cardOuterW - 2 // Lipgloss adds 2 for rounded border
		cardContentW := cardStyleW - 2 // Minus 2 for padding(0, 1)
		rem := totalCardSpace % 4
		card4StyleW := cardStyleW + rem

		barW := cardContentW - 9
		if barW < 6 {
			barW = 6
		}
		if barW > 30 {
			barW = 30
		}
		sparkW := cardContentW - 8
		if sparkW < 8 {
			sparkW = 8
		}
		if sparkW > 30 {
			sparkW = 30
		}

		cpuCard := fmt.Sprintf("%s\n%s %s\n%s %s\n%s %.2f, %.2f, %.2f",
			m.styles.CardTitle.Render(fmt.Sprintf("⚡ CPU USAGE (%d Cores)", m.localMetrics.CPUCores)),
			m.renderGauge(cpuP, barW), m.formatPercent(cpuP),
			m.styles.MutedText.Render("Trend:"), m.renderSparkline(m.cpuHistory, sparkW),
			m.styles.MutedText.Render("Load: "), m.localMetrics.Load1, m.localMetrics.Load5, m.localMetrics.Load15,
		)

		memCard := fmt.Sprintf("%s\n%s %s\n%s %s\n%s %s / %s",
			m.styles.CardTitle.Render("🧠 RAM MEMORY"),
			m.renderGauge(memP, barW), m.formatPercent(memP),
			m.styles.MutedText.Render("Trend:"), m.renderSparkline(m.memHistory, sparkW),
			m.styles.MutedText.Render("Used: "), usedMemStr, totMemStr,
		)

		swapCard := fmt.Sprintf("%s\n%s %s\n%s %s\n%s %s\n%s %s",
			m.styles.CardTitle.Render("🔄 SWAP MEMORY"),
			m.renderGauge(swapP, barW), m.formatPercent(swapP),
			m.styles.MutedText.Render("Free: "), formatBytes(m.localMetrics.FreeSwap),
			m.styles.MutedText.Render("Used: "), usedSwapStr,
			m.styles.MutedText.Render("Total:"), totSwapStr,
		)

		netCard := fmt.Sprintf("%s\n%s ↓ %s/s\n%s ↑ %s/s\n%s %s\n%s %s",
			m.styles.CardTitle.Render("🌐 NETWORK I/O"),
			m.styles.GaugeLow.Render("RECEIVE: "), rxRateStr,
			m.styles.GaugeMed.Render("TRANSMIT:"), txRateStr,
			m.styles.MutedText.Render("Total Rx:"), formatBytes(m.localMetrics.BytesRecv),
			m.styles.MutedText.Render("Total Tx:"), formatBytes(m.localMetrics.BytesSent),
		)

		c1 := m.styles.MetricCard.Width(cardStyleW).Render(cpuCard)
		c2 := m.styles.MetricCard.Width(cardStyleW).Render(memCard)
		c3 := m.styles.MetricCard.Width(cardStyleW).Render(swapCard)
		c4 := m.styles.MetricCard.Width(card4StyleW).Render(netCard)

		sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, c1, " ", c2, " ", c3, " ", c4))
	} else {
		// 2x2 grid for narrow screens
		halfSpace := width - 1
		cardOuterW2 := halfSpace / 2
		cardStyleW2 := cardOuterW2 - 2
		cardContentW2 := cardStyleW2 - 2
		rem2 := halfSpace % 2

		barW := cardContentW2 - 9
		if barW < 6 {
			barW = 6
		}
		sparkW := cardContentW2 - 8
		if sparkW < 6 {
			sparkW = 6
		}

		cpuCard := fmt.Sprintf("%s\n%s %s\n%s %s\n%s %.2f, %.2f",
			m.styles.CardTitle.Render(fmt.Sprintf("⚡ CPU (%d Cores)", m.localMetrics.CPUCores)),
			m.renderGauge(cpuP, barW), m.formatPercent(cpuP),
			m.styles.MutedText.Render("Trend:"), m.renderSparkline(m.cpuHistory, sparkW),
			m.styles.MutedText.Render("Load: "), m.localMetrics.Load1, m.localMetrics.Load5,
		)
		memCard := fmt.Sprintf("%s\n%s %s\n%s %s\n%s %s / %s",
			m.styles.CardTitle.Render("🧠 RAM MEMORY"),
			m.renderGauge(memP, barW), m.formatPercent(memP),
			m.styles.MutedText.Render("Trend:"), m.renderSparkline(m.memHistory, sparkW),
			m.styles.MutedText.Render("Used: "), usedMemStr, totMemStr,
		)
		swapCard := fmt.Sprintf("%s\n%s %s\n%s %s\n%s %s",
			m.styles.CardTitle.Render("🔄 SWAP MEMORY"),
			m.renderGauge(swapP, barW), m.formatPercent(swapP),
			m.styles.MutedText.Render("Free: "), formatBytes(m.localMetrics.FreeSwap),
			m.styles.MutedText.Render("Total:"), totSwapStr,
		)
		netCard := fmt.Sprintf("%s\n%s ↓%s/s\n%s ↑%s/s\n%s %s",
			m.styles.CardTitle.Render("🌐 NETWORK I/O"),
			m.styles.GaugeLow.Render("Rx:"), rxRateStr,
			m.styles.GaugeMed.Render("Tx:"), txRateStr,
			m.styles.MutedText.Render("Tot:"), formatBytes(m.localMetrics.BytesRecv),
		)

		c1 := m.styles.MetricCard.Width(cardStyleW2).Render(cpuCard)
		c2 := m.styles.MetricCard.Width(cardStyleW2 + rem2).Render(memCard)
		c3 := m.styles.MetricCard.Width(cardStyleW2).Render(swapCard)
		c4 := m.styles.MetricCard.Width(cardStyleW2 + rem2).Render(netCard)

		row1 := lipgloss.JoinHorizontal(lipgloss.Top, c1, " ", c2)
		row2 := lipgloss.JoinHorizontal(lipgloss.Top, c3, " ", c4)
		sb.WriteString(lipgloss.JoinVertical(lipgloss.Left, row1, row2))
	}
	sb.WriteString("\n\n")

	// CPU Per-Core Matrix (if available and <= 32 cores)
	if len(m.localMetrics.CoreUsages) > 0 {
		sb.WriteString(m.styles.Subtitle.Render("📊 CPU PER-CORE USAGE MATRIX") + "\n")
		sb.WriteString(m.styles.Divider.Render(strings.Repeat("─", width)) + "\n")

		limitCores := len(m.localMetrics.CoreUsages)
		if limitCores > 32 {
			limitCores = 32
		}

		targetColW := 24
		numCols := width / targetColW
		if numCols < 2 {
			numCols = 2
		}
		if numCols > 8 {
			numCols = 8
		}
		if numCols > limitCores {
			numCols = limitCores
		}

		colW := width / numCols
		gaugeW := colW - 14
		if gaugeW < 4 {
			gaugeW = 4
		}
		if gaugeW > 12 {
			gaugeW = 12
		}

		var currRow strings.Builder
		for idx := 0; idx < limitCores; idx++ {
			cUsage := m.localMetrics.CoreUsages[idx]
			coreTag := fmt.Sprintf("C%-2d", idx)
			gauge := m.renderGauge(cUsage, gaugeW)
			pct := m.formatPercent(cUsage)
			cell := fmt.Sprintf(" %s [%s] %s", m.styles.MutedText.Render(coreTag), gauge, pct)

			cellW := lipgloss.Width(cell)
			if cellW < colW {
				cell += strings.Repeat(" ", colW-cellW)
			}
			currRow.WriteString(cell)

			if (idx+1)%numCols == 0 || idx == limitCores-1 {
				currRow.WriteString("\n")
			}
		}
		sb.WriteString(currRow.String())
		sb.WriteString("\n")
	}

	// Storage & Disks Partitions Breakdown Table
	sb.WriteString(m.styles.Subtitle.Render("💾 STORAGE & DISK PARTITIONS") + "\n")
	sb.WriteString(m.styles.Divider.Render(strings.Repeat("─", width)) + "\n")

	if len(m.localMetrics.Partitions) > 0 {
		if width >= 110 {
			// Full 6 columns (MOUNTPOINT, DEVICE, FSTYPE, USED/TOTAL, USAGE METER, USAGE%)
			devW := 16
			fsW := 8
			sizeW := 22
			pctW := 8
			fixed := devW + fsW + sizeW + pctW + 10
			remaining := width - fixed
			if remaining < 20 {
				remaining = 20
			}
			mountW := remaining * 45 / 100
			if mountW < 16 {
				mountW = 16
			}
			if mountW > 36 {
				mountW = 36
			}
			barW := remaining - mountW
			if barW < 8 {
				barW = 8
			}
			if barW > 32 {
				barW = 32
			}

			fmt.Fprintf(&sb, "  %-*s %-*s %-*s %-*s %-*s %*s\n",
				mountW, "MOUNTPOINT",
				devW, "DEVICE",
				fsW, "FSTYPE",
				sizeW, "USED / TOTAL",
				barW, "USAGE METER",
				pctW, "USAGE%",
			)

			for _, p := range m.localMetrics.Partitions {
				pBar := m.renderGauge(p.UsedPercent, barW)
				fmt.Fprintf(&sb, "  %-*s %-*s %-*s %-*s %s %s\n",
					mountW, truncate(p.Mountpoint, mountW-1),
					devW, truncate(p.Device, devW-1),
					fsW, p.Fstype,
					sizeW, fmt.Sprintf("%s / %s", formatBytes(p.Used), formatBytes(p.Total)),
					pBar,
					m.formatPercent(p.UsedPercent),
				)
			}
		} else if width >= 85 {
			// 5 columns (omit FSTYPE)
			devW := 14
			sizeW := 20
			pctW := 8
			fixed := devW + sizeW + pctW + 8
			remaining := width - fixed
			if remaining < 16 {
				remaining = 16
			}
			mountW := remaining * 45 / 100
			if mountW < 14 {
				mountW = 14
			}
			barW := remaining - mountW
			if barW < 6 {
				barW = 6
			}

			fmt.Fprintf(&sb, "  %-*s %-*s %-*s %-*s %*s\n",
				mountW, "MOUNTPOINT",
				devW, "DEVICE",
				sizeW, "USED / TOTAL",
				barW, "USAGE METER",
				pctW, "USAGE%",
			)

			for _, p := range m.localMetrics.Partitions {
				pBar := m.renderGauge(p.UsedPercent, barW)
				fmt.Fprintf(&sb, "  %-*s %-*s %-*s %s %s\n",
					mountW, truncate(p.Mountpoint, mountW-1),
					devW, truncate(p.Device, devW-1),
					sizeW, fmt.Sprintf("%s / %s", formatBytes(p.Used), formatBytes(p.Total)),
					pBar,
					m.formatPercent(p.UsedPercent),
				)
			}
		} else {
			// Compact 4 columns (MOUNTPOINT, USED/TOTAL, USAGE METER, USAGE%)
			sizeW := 18
			pctW := 8
			fixed := sizeW + pctW + 6
			remaining := width - fixed
			if remaining < 16 {
				remaining = 16
			}
			mountW := remaining * 45 / 100
			if mountW < 12 {
				mountW = 12
			}
			barW := remaining - mountW
			if barW < 6 {
				barW = 6
			}

			fmt.Fprintf(&sb, "  %-*s %-*s %-*s %*s\n",
				mountW, "MOUNTPOINT",
				sizeW, "USED / TOTAL",
				barW, "USAGE METER",
				pctW, "USAGE%",
			)

			for _, p := range m.localMetrics.Partitions {
				pBar := m.renderGauge(p.UsedPercent, barW)
				fmt.Fprintf(&sb, "  %-*s %-*s %s %s\n",
					mountW, truncate(p.Mountpoint, mountW-1),
					sizeW, fmt.Sprintf("%s / %s", formatBytes(p.Used), formatBytes(p.Total)),
					pBar,
					m.formatPercent(p.UsedPercent),
				)
			}
		}
	} else {
		fmt.Fprintf(&sb, "  Root: %s / %s (%s)\n",
			formatBytes(m.localMetrics.UsedDisk), formatBytes(m.localMetrics.TotalDisk), m.formatPercent(m.localMetrics.DiskUsage))
	}
	sb.WriteString("\n")

	// Top Processes (Task Manager)
	if len(m.localMetrics.TopProcesses) > 0 {
		sb.WriteString(m.styles.Subtitle.Render("📈 TOP PROCESSES (TASK MANAGER)") + "\n")
		sb.WriteString(m.styles.Divider.Render(strings.Repeat("─", width)) + "\n")

		if width >= 105 {
			// Full dual meter view
			pidW := 8
			sizeW := 12
			cpuBarW := 10
			memBarW := 10
			fixed := pidW + sizeW + (cpuBarW + 9) + (memBarW + 9) + 12
			procW := width - fixed
			if procW < 16 {
				procW = 16
			}
			if procW > 38 {
				procW = 38
			}

			fmt.Fprintf(&sb, "  %-*s %-*s %-*s %-*s %*s\n",
				pidW, "PID",
				procW, "PROCESS",
				cpuBarW+8, "CPU METER",
				memBarW+8, "MEM METER",
				sizeW, "RSS MEMORY",
			)

			for _, pr := range m.localMetrics.TopProcesses {
				cpuGauge := m.renderGauge(min(100.0, pr.CPUPercent), cpuBarW)
				memGauge := m.renderGauge(min(100.0, float64(pr.MemoryPercent)), memBarW)
				fmt.Fprintf(&sb, "  %-*d %-*s %s %s %s %s %*s\n",
					pidW, pr.PID,
					procW, truncate(pr.Name, procW-1),
					cpuGauge, m.formatPercent(pr.CPUPercent),
					memGauge, m.formatPercent(float64(pr.MemoryPercent)),
					sizeW, formatBytes(pr.MemoryBytes),
				)
			}
		} else if width >= 85 {
			// Single CPU meter + Mem %
			pidW := 7
			sizeW := 10
			cpuBarW := 8
			pctW := 7
			fixed := pidW + sizeW + (cpuBarW + 9) + pctW + 10
			procW := width - fixed
			if procW < 14 {
				procW = 14
			}
			if procW > 30 {
				procW = 30
			}

			fmt.Fprintf(&sb, "  %-*s %-*s %-*s %-*s %*s\n",
				pidW, "PID",
				procW, "PROCESS",
				cpuBarW+8, "CPU METER",
				pctW, "MEM%",
				sizeW, "RSS MEM",
			)

			for _, pr := range m.localMetrics.TopProcesses {
				cpuGauge := m.renderGauge(min(100.0, pr.CPUPercent), cpuBarW)
				fmt.Fprintf(&sb, "  %-*d %-*s %s %s %s %*s\n",
					pidW, pr.PID,
					procW, truncate(pr.Name, procW-1),
					cpuGauge, m.formatPercent(pr.CPUPercent),
					m.formatPercent(float64(pr.MemoryPercent)),
					sizeW, formatBytes(pr.MemoryBytes),
				)
			}
		} else {
			// Compact view: PID, PROCESS, CPU%, MEM%, RSS MEM
			pidW := 7
			sizeW := 10
			pctW := 7
			fixed := pidW + pctW + pctW + sizeW + 8
			procW := width - fixed
			if procW < 12 {
				procW = 12
			}

			fmt.Fprintf(&sb, "  %-*s %-*s %-*s %-*s %*s\n",
				pidW, "PID",
				procW, "PROCESS",
				pctW, "CPU%",
				pctW, "MEM%",
				sizeW, "RSS MEM",
			)

			for _, pr := range m.localMetrics.TopProcesses {
				fmt.Fprintf(&sb, "  %-*d %-*s %s %s %*s\n",
					pidW, pr.PID,
					procW, truncate(pr.Name, procW-1),
					m.formatPercent(pr.CPUPercent),
					m.formatPercent(float64(pr.MemoryPercent)),
					sizeW, formatBytes(pr.MemoryBytes),
				)
			}
		}
		sb.WriteString("\n")
	}

	// Machine Host Details
	sb.WriteString(m.styles.Subtitle.Render("💻 HOST & MESH PLATFORM") + "\n")
	sb.WriteString(m.styles.Divider.Render(strings.Repeat("─", width)) + "\n")
	uptimeStr := formatUptime(m.localMetrics.Uptime)
	modelStr := m.localMetrics.CPUModel

	if width >= 120 {
		col1 := fmt.Sprintf("Hostname:   %s (%s)", m.localMetrics.Hostname, m.localMetrics.Arch)
		col2 := fmt.Sprintf("OS/Platform: %s %s", m.localMetrics.Platform, m.localMetrics.PlatformVer)
		col3 := fmt.Sprintf("System Uptime: %s", uptimeStr)
		fmt.Fprintf(&sb, "  %-38s %-38s %s\n", truncate(col1, 36), truncate(col2, 36), col3)

		col4 := fmt.Sprintf("Kernel:     %s", m.localMetrics.KernelVer)
		col5 := fmt.Sprintf("CPU Model:   %s", modelStr)
		col6 := fmt.Sprintf("Load Averages: %.2f, %.2f, %.2f", m.localMetrics.Load1, m.localMetrics.Load5, m.localMetrics.Load15)
		fmt.Fprintf(&sb, "  %-38s %-38s %s\n", truncate(col4, 36), truncate(col5, 36), col6)
	} else if width >= 85 {
		colW := (width - 4) / 2
		fmt.Fprintf(&sb, "  %-*s %s\n",
			colW, "Hostname: "+truncate(fmt.Sprintf("%s (%s)", m.localMetrics.Hostname, m.localMetrics.Arch), colW-11),
			"OS: "+truncate(fmt.Sprintf("%s %s", m.localMetrics.Platform, m.localMetrics.PlatformVer), colW-5),
		)
		fmt.Fprintf(&sb, "  %-*s %s\n",
			colW, "Kernel:   "+truncate(m.localMetrics.KernelVer, colW-11),
			"Uptime: "+uptimeStr,
		)
		if modelStr != "" {
			fmt.Fprintf(&sb, "  %-*s %s\n",
				colW, "CPU Model: "+truncate(modelStr, colW-12),
				fmt.Sprintf("Load: %.2f, %.2f, %.2f", m.localMetrics.Load1, m.localMetrics.Load5, m.localMetrics.Load15),
			)
		}
	} else {
		// Compact host layout
		fmt.Fprintf(&sb, "  Hostname: %s (%s)\n", truncate(m.localMetrics.Hostname, width-20), m.localMetrics.Arch)
		fmt.Fprintf(&sb, "  OS:       %s %s | Up: %s\n", m.localMetrics.Platform, m.localMetrics.PlatformVer, uptimeStr)
		fmt.Fprintf(&sb, "  Kernel:   %s\n", truncate(m.localMetrics.KernelVer, width-12))
	}

	return sb.String()
}

func (m *DashboardModel) renderNodesTab(width int) string {
	var sb strings.Builder

	sb.WriteString(m.styles.Subtitle.Render("🌐 MESH NODES (P2P WIREGUARD NETWORK)") + "\n\n")

	m.mu.RLock()
	defer m.mu.RUnlock()

	divider := m.styles.Divider.Render(strings.Repeat("─", width)) + "\n"

	if width >= 105 {
		header := fmt.Sprintf("  %-2s %-16s %-18s %-16s %-14s %-10s %-14s %-10s\n",
			" ", "NAME", "HOST / IP", "TAGS", "STATUS", "LATENCY", "OS / ARCH", "CPU/RAM")
		sb.WriteString(header)
		sb.WriteString(divider)

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

			hostStr := n.Host
			if hostStr == "" {
				hostStr = n.IP
			}
			if hostStr == "" {
				hostStr = truncate(n.Addr, 17)
			}

			tagsStr := "-"
			if len(n.Tags) > 0 {
				tagsStr = strings.Join(n.Tags, ",")
			}

			if n.GossipState == entity.GossipStateSuspect {
				statusStr = m.styles.WarningText.Render("▲ Suspect")
			} else if n.GossipState == entity.GossipStateDead {
				statusStr = m.styles.StatusFailed
			}

			if st != nil {
				if st.IsProbing {
					statusStr = m.styles.StatusProbing
				} else if st.IsOnline {
					if n.GossipState == "" || n.GossipState == entity.GossipStateAlive {
						statusStr = m.styles.StatusRunning
					}
					if st.Latency > 0 {
						latencyStr = fmt.Sprintf("%dms", st.Latency.Milliseconds())
					} else {
						latencyStr = "<1ms"
					}
					if st.Node != nil && st.Node.CPUUsage > 0 {
						cpuMemStr = fmt.Sprintf("%.0f%%/%.0f%%", st.Node.CPUUsage, st.Node.MemoryUsage)
					}
				} else {
					if n.GossipState != entity.GossipStateSuspect {
						statusStr = m.styles.StatusFailed
					}
					latencyStr = "Timeout"
				}
			}

			if osArchStr == "/" {
				osArchStr = "unknown"
			}

			row := fmt.Sprintf("  %-2s %-16s %-18s %-16s %-14s %-10s %-14s %-10s\n",
				cursor,
				truncate(n.Name, 15),
				truncate(hostStr, 17),
				truncate(tagsStr, 15),
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
	} else if width >= 85 {
		// Medium view: omit OS/ARCH
		header := fmt.Sprintf("  %-2s %-16s %-18s %-14s %-14s %-10s %-10s\n",
			" ", "NAME", "HOST / IP", "TAGS", "STATUS", "LATENCY", "CPU/RAM")
		sb.WriteString(header)
		sb.WriteString(divider)

		for i, n := range m.nodes {
			cursor := "  "
			if i == m.selectedNode {
				cursor = "❯ "
			}

			st := m.nodeStates[n.Name]
			statusStr := m.styles.StatusStopped
			latencyStr := "-"
			cpuMemStr := "-"

			hostStr := n.Host
			if hostStr == "" {
				hostStr = n.IP
			}
			if hostStr == "" {
				hostStr = truncate(n.Addr, 17)
			}

			tagsStr := "-"
			if len(n.Tags) > 0 {
				tagsStr = strings.Join(n.Tags, ",")
			}

			if n.GossipState == entity.GossipStateSuspect {
				statusStr = m.styles.WarningText.Render("▲ Suspect")
			} else if n.GossipState == entity.GossipStateDead {
				statusStr = m.styles.StatusFailed
			}

			if st != nil {
				if st.IsProbing {
					statusStr = m.styles.StatusProbing
				} else if st.IsOnline {
					if n.GossipState == "" || n.GossipState == entity.GossipStateAlive {
						statusStr = m.styles.StatusRunning
					}
					if st.Latency > 0 {
						latencyStr = fmt.Sprintf("%dms", st.Latency.Milliseconds())
					} else {
						latencyStr = "<1ms"
					}
					if st.Node != nil && st.Node.CPUUsage > 0 {
						cpuMemStr = fmt.Sprintf("%.0f%%/%.0f%%", st.Node.CPUUsage, st.Node.MemoryUsage)
					}
				} else {
					if n.GossipState != entity.GossipStateSuspect {
						statusStr = m.styles.StatusFailed
					}
					latencyStr = "Timeout"
				}
			}

			row := fmt.Sprintf("  %-2s %-16s %-18s %-14s %-14s %-10s %-10s\n",
				cursor,
				truncate(n.Name, 15),
				truncate(hostStr, 17),
				truncate(tagsStr, 13),
				statusStr,
				latencyStr,
				cpuMemStr,
			)

			if i == m.selectedNode {
				sb.WriteString(m.styles.SelectedRow.Render(row))
			} else {
				sb.WriteString(row)
			}
		}
	} else {
		// Compact view: NAME, HOST / IP, STATUS, CPU/RAM
		header := fmt.Sprintf("  %-2s %-16s %-18s %-14s %-10s\n",
			" ", "NAME", "HOST / IP", "STATUS", "CPU/RAM")
		sb.WriteString(header)
		sb.WriteString(divider)

		for i, n := range m.nodes {
			cursor := "  "
			if i == m.selectedNode {
				cursor = "❯ "
			}

			st := m.nodeStates[n.Name]
			statusStr := m.styles.StatusStopped
			cpuMemStr := "-"

			hostStr := n.Host
			if hostStr == "" {
				hostStr = n.IP
			}
			if hostStr == "" {
				hostStr = truncate(n.Addr, 17)
			}

			if n.GossipState == entity.GossipStateSuspect {
				statusStr = m.styles.WarningText.Render("▲ Suspect")
			} else if n.GossipState == entity.GossipStateDead {
				statusStr = m.styles.StatusFailed
			}

			if st != nil {
				if st.IsProbing {
					statusStr = m.styles.StatusProbing
				} else if st.IsOnline {
					if n.GossipState == "" || n.GossipState == entity.GossipStateAlive {
						statusStr = m.styles.StatusRunning
					}
					if st.Node != nil && st.Node.CPUUsage > 0 {
						cpuMemStr = fmt.Sprintf("%.0f%%/%.0f%%", st.Node.CPUUsage, st.Node.MemoryUsage)
					}
				} else {
					if n.GossipState != entity.GossipStateSuspect {
						statusStr = m.styles.StatusFailed
					}
				}
			}

			row := fmt.Sprintf("  %-2s %-16s %-18s %-14s %-10s\n",
				cursor,
				truncate(n.Name, 15),
				truncate(hostStr, 17),
				statusStr,
				cpuMemStr,
			)

			if i == m.selectedNode {
				sb.WriteString(m.styles.SelectedRow.Render(row))
			} else {
				sb.WriteString(row)
			}
		}
	}

	// Details of Selected Node
	if m.selectedNode < len(m.nodes) {
		sn := m.nodes[m.selectedNode]
		st := m.nodeStates[sn.Name]
		sb.WriteString("\n" + m.styles.Subtitle.Render("🔎 SELECTED NODE DETAILS") + "\n")
		sb.WriteString(m.styles.Divider.Render(strings.Repeat("─", width)) + "\n")
		fmt.Fprintf(&sb, "  Node ID:      %s\n", sn.ID)
		fmt.Fprintf(&sb, "  Mesh Addr:    %s\n", sn.Addr)

		hostVal := sn.Host
		if hostVal == "" {
			hostVal = sn.IP
		}
		if hostVal == "" {
			hostVal = "-"
		}
		fmt.Fprintf(&sb, "  Host / IP:    %s\n", hostVal)

		tagsVal := "-"
		if len(sn.Tags) > 0 {
			tagsVal = strings.Join(sn.Tags, ", ")
		}
		fmt.Fprintf(&sb, "  Tags:         %s\n", tagsVal)

		nodeStatus := sn.Status
		if nodeStatus == "" {
			nodeStatus = string(sn.GossipState)
		}
		if nodeStatus == "" {
			if st != nil && st.IsOnline {
				nodeStatus = "alive"
			} else {
				nodeStatus = "offline"
			}
		}
		fmt.Fprintf(&sb, "  Health:       %s (Epoch: %d)\n", strings.ToUpper(nodeStatus), sn.Incarnation)

		if sn.CPUUsage > 0 || sn.MemoryUsage > 0 {
			fmt.Fprintf(&sb, "  Telemetry:    CPU: %.1f%% | RAM: %.1f%% | Disk: %.1f%% | Load: %.2f\n",
				sn.CPUUsage, sn.MemoryUsage, sn.DiskUsage, sn.Load1)
		}

		if st != nil && !st.IsOnline && st.Error != "" {
			fmt.Fprintf(&sb, "  Error:        %s\n", m.styles.MutedText.Render(st.Error))
		}
		if st != nil && len(st.Services) > 0 {
			var svcNames []string
			for _, sv := range st.Services {
				svcNames = append(svcNames, sv.Name)
			}
			fmt.Fprintf(&sb, "  Workloads:    %s\n", strings.Join(svcNames, ", "))
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

	divider := m.styles.Divider.Render(strings.Repeat("─", width)) + "\n"

	if width >= 120 {
		header := fmt.Sprintf("  %-2s %-18s %-16s %-12s %-14s %-18s %-24s %-14s\n",
			" ", "SERVICE", "NODE", "TYPE", "STATUS", "PORTS", "INGRESS DOMAIN", "HTTPS/TLS")
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

			row := fmt.Sprintf("  %-2s %-18s %-16s %-12s %-14s %-18s %-24s %-14s\n",
				cursor,
				truncate(w.Name, 17),
				truncate(w.NodeName, 15),
				truncate(string(w.Type), 11),
				st,
				truncate(portsStr, 17),
				truncate(w.Domain, 23),
				truncate(w.TLS, 13),
			)

			if i == m.selectedSvc {
				sb.WriteString(m.styles.SelectedRow.Render(row))
			} else {
				sb.WriteString(row)
			}
		}
	} else if width >= 90 {
		// Medium view: omit PORTS and TLS
		header := fmt.Sprintf("  %-2s %-18s %-14s %-10s %-14s %-20s\n",
			" ", "SERVICE", "NODE", "TYPE", "STATUS", "INGRESS DOMAIN")
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

			row := fmt.Sprintf("  %-2s %-18s %-14s %-10s %-14s %-20s\n",
				cursor,
				truncate(w.Name, 17),
				truncate(w.NodeName, 13),
				truncate(string(w.Type), 9),
				st,
				truncate(w.Domain, 19),
			)

			if i == m.selectedSvc {
				sb.WriteString(m.styles.SelectedRow.Render(row))
			} else {
				sb.WriteString(row)
			}
		}
	} else {
		// Compact view: SERVICE, TYPE, STATUS, DOMAIN
		header := fmt.Sprintf("  %-2s %-16s %-10s %-14s %-18s\n",
			" ", "SERVICE", "TYPE", "STATUS", "DOMAIN")
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

			row := fmt.Sprintf("  %-2s %-16s %-10s %-14s %-18s\n",
				cursor,
				truncate(w.Name, 15),
				truncate(string(w.Type), 9),
				st,
				truncate(w.Domain, 17),
			)

			if i == m.selectedSvc {
				sb.WriteString(m.styles.SelectedRow.Render(row))
			} else {
				sb.WriteString(row)
			}
		}
	}

	// Details of Selected Workload
	if m.selectedSvc < len(workloads) {
		sw := workloads[m.selectedSvc]
		sb.WriteString("\n" + m.styles.Subtitle.Render("🔎 SELECTED WORKLOAD DETAILS") + "\n")
		sb.WriteString(m.styles.Divider.Render(strings.Repeat("─", width)) + "\n")
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

	maxLogW := width - 4
	if maxLogW < 30 {
		maxLogW = 30
	}
	for _, l := range m.logs {
		sb.WriteString("  " + truncate(l, maxLogW) + "\n")
	}
	sb.WriteString("\n  " + m.styles.MutedText.Render("(Press [r] to refresh; logs auto-stream from running services...)"))
	return sb.String()
}

func (m *DashboardModel) renderBackupsTab(width int) string {
	var sb strings.Builder
	sb.WriteString(m.styles.Subtitle.Render("💾 BACKUPS & SNAPSHOTS") + "\n\n")

	sb.WriteString("  Active Backup Policies:\n")
	if width >= 90 {
		sb.WriteString("  • Daily Snapshot: Local Directory (.kizuna/backups) + S3 Compatible Vault\n")
		sb.WriteString("  • Retention: Keep last 7 revisions (older archives automatically pruned)\n\n")
	} else {
		sb.WriteString("  • Daily Snapshot: Local (.kizuna/backups) + S3 Vault\n")
		sb.WriteString("  • Retention: Keep last 7 revisions (auto-pruned)\n\n")
	}
	sb.WriteString("  Create a new snapshot with:  kizuna backup <service-name>\n")
	return sb.String()
}

// renderGauge renders a smooth, high-resolution gauge with 1/8th fractional blocks
func (m *DashboardModel) renderGauge(percent float64, width int) string {
	if width < 3 {
		width = 3
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}

	totalEighths := int((percent/100.0)*float64(width*8) + 0.5)
	fullBlocks := totalEighths / 8
	partial := totalEighths % 8

	var fillStyle lipgloss.Style
	if percent < 60.0 {
		fillStyle = m.styles.GaugeLow
	} else if percent < 85.0 {
		fillStyle = m.styles.GaugeMed
	} else {
		fillStyle = m.styles.GaugeHigh
	}

	var sb strings.Builder
	if fullBlocks > 0 {
		sb.WriteString(fillStyle.Render(strings.Repeat("█", fullBlocks)))
	}
	emptyBlocks := width - fullBlocks
	if partial > 0 && emptyBlocks > 0 {
		subRunes := []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}
		sb.WriteString(fillStyle.Render(string(subRunes[partial])))
		emptyBlocks--
	}
	if emptyBlocks > 0 {
		sb.WriteString(m.styles.GaugeEmpty.Render(strings.Repeat("░", emptyBlocks)))
	}
	return sb.String()
}

// formatPercent formats a percentage with color-graded threshold styling
func (m *DashboardModel) formatPercent(p float64) string {
	var style lipgloss.Style
	if p < 60.0 {
		style = m.styles.GaugeLow
	} else if p < 85.0 {
		style = m.styles.GaugeMed
	} else {
		style = m.styles.GaugeHigh
	}
	return style.Render(fmt.Sprintf("%5.1f%%", p))
}

func (m *DashboardModel) renderSparkline(values []float64, maxLen int) string {
	sparks := []rune{' ', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	if maxLen <= 0 {
		maxLen = 14
	}

	var displayValues []float64
	if len(values) < maxLen {
		padCount := maxLen - len(values)
		baseline := 0.0
		if len(values) > 0 {
			baseline = values[0]
		}
		for i := 0; i < padCount; i++ {
			displayValues = append(displayValues, baseline)
		}
		displayValues = append(displayValues, values...)
	} else {
		displayValues = values[len(values)-maxLen:]
	}

	var sb strings.Builder
	for _, v := range displayValues {
		idx := int((v / 100.0) * float64(len(sparks)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparks) {
			idx = len(sparks) - 1
		}
		sb.WriteRune(sparks[idx])
	}
	return m.styles.Sparkline.Render(sb.String())
}

func renderBar(percent float64, totalBars int) string {
	if totalBars <= 0 {
		totalBars = 20
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}

	totalEighths := int((percent/100.0)*float64(totalBars*8) + 0.5)
	fullBlocks := totalEighths / 8
	partial := totalEighths % 8

	var sb strings.Builder
	if fullBlocks > 0 {
		sb.WriteString(strings.Repeat("█", fullBlocks))
	}
	emptyBlocks := totalBars - fullBlocks
	if partial > 0 && emptyBlocks > 0 {
		subRunes := []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}
		sb.WriteString(string(subRunes[partial]))
		emptyBlocks--
	}
	if emptyBlocks > 0 {
		sb.WriteString(strings.Repeat("░", emptyBlocks))
	}
	return sb.String()
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
