package presenter

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/telemetry"
)

func TestDashboardAdaptiveView(t *testing.T) {
	nodes := []*entity.Node{
		{ID: "node-1", Name: "alpha-worker", Addr: "10.0.0.2", OS: "linux", Arch: "amd64"},
		{ID: "node-2", Name: "beta-edge", Addr: "10.0.0.3", OS: "linux", Arch: "arm64"},
	}

	services := []*entity.Service{
		{
			Name:     "web-frontend",
			Type:     entity.TypeDocker,
			State:    entity.StateRunning,
			Ports:    []string{"80:8080", "443:8443"},
			Replicas: 2,
			Ingress: &entity.IngressConfig{
				Domain: "app.homelab.local",
				TLS:    "internal",
			},
			UpdatedAt: time.Now(),
		},
	}

	m := NewDashboardWithOptions(DashboardOptions{
		ActiveEnv:          "production",
		Theme:              entity.ResolveTheme("catppuccin", nil),
		Nodes:              nodes,
		ConfiguredServices: services,
	})

	// Inject rich mock telemetry
	m.localMetrics = &telemetry.Metrics{
		Hostname:    "kizuna-node",
		Platform:    "omarchy",
		PlatformVer: "4.0.4",
		Arch:        "amd64",
		KernelVer:   "6.8.0-kizuna",
		CPUModel:    "AMD Ryzen 9 7950X 16-Core",
		CPUCores:    16,
		CPUUsage:    42.5,
		CoreUsages:  []float64{10, 25, 60, 90, 15, 30, 45, 80},
		MemoryUsage: 55.0,
		UsedMemory:  16 * 1024 * 1024 * 1024,
		TotalMemory: 32 * 1024 * 1024 * 1024,
		SwapUsage:   12.0,
		UsedSwap:    1 * 1024 * 1024 * 1024,
		TotalSwap:   8 * 1024 * 1024 * 1024,
		RxRate:      5 * 1024 * 1024,
		TxRate:      2 * 1024 * 1024,
		Partitions: []telemetry.PartitionInfo{
			{Mountpoint: "/mnt/storage/media", Device: "/dev/nvme0n1p2", Fstype: "ext4", Used: 500 * 1024 * 1024 * 1024, Total: 1000 * 1024 * 1024 * 1024, UsedPercent: 50.0},
		},
		TopProcesses: []telemetry.ProcessInfo{
			{PID: 1234, Name: "kizuna-engine", CPUPercent: 35.5, MemoryPercent: 8.5, MemoryBytes: 512 * 1024 * 1024},
		},
	}

	widths := []int{70, 80, 95, 110, 130, 160, 200}
	tabs := []string{"1", "2", "3", "4", "5"}

	for _, tabKey := range tabs {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tabKey)})

		for _, w := range widths {
			m.Update(tea.WindowSizeMsg{Width: w, Height: 45})
			out := m.View()

			if !strings.Contains(out, "KIZUNA DASHBOARD") {
				t.Errorf("tab %s, width %d: expected dashboard title", tabKey, w)
			}

			lines := strings.Split(out, "\n")
			for i, l := range lines {
				lw := lipgloss.Width(l)
				if lw > w+2 {
					t.Errorf("tab %s, width %d: line %d exceeds limit (actual %d > max %d): %q", tabKey, w, i, lw, w+2, l)
				}
			}
		}
	}
}

func TestDashboardGaugeAndSparkline(t *testing.T) {
	m := NewDashboardWithOptions(DashboardOptions{
		ActiveEnv: "production",
		Theme:     entity.ResolveTheme("catppuccin", nil),
	})

	// Test fractional gauges across percentages
	for _, p := range []float64{0, 12.5, 33.3, 50, 75, 95.5, 100} {
		gauge := m.renderGauge(p, 20)
		w := lipgloss.Width(gauge)
		if w != 20 {
			t.Errorf("renderGauge(%.1f, 20) produced width %d, expected 20", p, w)
		}
	}

	// Test sparkline padding & rendering
	sp := m.renderSparkline([]float64{10, 20, 30}, 16)
	if lipgloss.Width(sp) != 16 {
		t.Errorf("renderSparkline with maxLen 16 produced width %d", lipgloss.Width(sp))
	}
}

func TestDashboardNodeStatusSync(t *testing.T) {
	node := &entity.Node{
		ID:          "node-1",
		Name:        "worker-1",
		Addr:        "192.168.1.18:19800",
		Status:      "alive",
		GossipState: entity.GossipStateAlive,
	}

	m := NewDashboardWithOptions(DashboardOptions{
		Nodes: []*entity.Node{node},
		Theme: entity.ResolveTheme("catppuccin", nil),
	})

	// Case 1: Probe failed with error
	m.nodeStates["worker-1"] = &NodeProbeState{
		IsOnline:  false,
		IsProbing: false,
		Error:     "context deadline exceeded",
	}

	badge, label := m.getNodeStatus(node, m.nodeStates["worker-1"])
	if label == "ALIVE" {
		t.Errorf("expected UNREACHABLE status when probe failed, got label %s", label)
	}
	if !strings.Contains(badge, "Unreachable") {
		t.Errorf("expected Unreachable badge when probe failed, got %s", badge)
	}

	// Verify renderNodesTab output
	tabOut := m.renderNodesTab(120)
	if strings.Contains(tabOut, "Health:       ALIVE") {
		t.Errorf("Health in details pane must not show ALIVE when probe failed: %s", tabOut)
	}
	if !strings.Contains(tabOut, "Unreachable") {
		t.Errorf("expected Unreachable in table and details: %s", tabOut)
	}
	if !strings.Contains(tabOut, "context deadline exceeded") {
		t.Errorf("expected error message in details pane: %s", tabOut)
	}

	// Case 2: Online node
	m.nodeStates["worker-1"] = &NodeProbeState{
		IsOnline:  true,
		IsProbing: false,
		Latency:   4 * time.Millisecond,
	}
	badgeOnline, labelOnline := m.getNodeStatus(node, m.nodeStates["worker-1"])
	if labelOnline != "ALIVE" {
		t.Errorf("expected ALIVE when probe succeeded, got %s", labelOnline)
	}
	if !strings.Contains(badgeOnline, "Online") {
		t.Errorf("expected Online badge, got %s", badgeOnline)
	}
}

func TestDashboardSelectedRowAlignment(t *testing.T) {
	node := &entity.Node{
		ID:          "node-1",
		Name:        "worker-1",
		Addr:        "192.168.1.18:19800",
		OS:          "darwin",
		Arch:        "arm64",
		Status:      "offline",
		GossipState: entity.GossipStateDead,
	}

	m := NewDashboardWithOptions(DashboardOptions{
		Nodes: []*entity.Node{node},
		Theme: entity.ResolveTheme("catppuccin", nil),
	})
	m.nodeStates["worker-1"] = &NodeProbeState{
		IsOnline:  false,
		IsProbing: false,
		Error:     "context deadline exceeded",
	}

	// Render wide view
	out := m.renderNodesTab(120)

	// Verify no glued words like "UnreachableTimeout"
	if strings.Contains(out, "UnreachableTimeout") {
		t.Errorf("columns are misaligned/mashed: %s", out)
	}

	// Verify trailing newline is not styled inside selection block (no trailing box)
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		if strings.HasSuffix(l, " \x1b[0m") && !strings.Contains(l, "─") {
			// Ensure no floating block artifact
			if lipgloss.Width(l) > 120 {
				t.Errorf("line exceeds expected width: %q", l)
			}
		}
	}
}

