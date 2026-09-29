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
