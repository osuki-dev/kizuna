package presenter

import (
	"strings"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

func TestFormatNodeAddr(t *testing.T) {
	// 1. Short address or standard IP:port
	shortAddr := "192.168.1.100:19800"
	if got := FormatNodeAddr(shortAddr, false); got != shortAddr {
		t.Fatalf("expected short address unchanged, got: %s", got)
	}

	// 2. Long 230-char tailcat mesh address
	longAddr := "tcpGFwWCAAw5HcHmWwJc38SeFH1sYfAxnau6DyFasWpd__iZsfE2FrWCD1R6vKA4mHNhvjLE9AEwPfHI6noosUK4pqAjn-eH-eKWFxWCDFRzm5w0gFvleZc5LDOg7PcNDdr-xlT4Uc75WEjl27fmFygaFhToGjYWhudGMzMDJhLmlwbi5kZXZhNG0yMDguMTExLjM5LjM4YTZzMjYwNzpmNzQwOjA6M2Y6OjcyMA"
	compact := FormatNodeAddr(longAddr, false)
	expectedCompact := "tcpGFwWCAA...OjcyMA"
	if compact != expectedCompact {
		t.Fatalf("expected middle truncated '%s', got: '%s'", expectedCompact, compact)
	}

	// 3. Wide mode returns full address
	full := FormatNodeAddr(longAddr, true)
	if full != longAddr {
		t.Fatalf("expected full address in wide mode, got: '%s'", full)
	}
}

func TestGossipPill_OfflineSafety(t *testing.T) {
	ui := NewUI("catppuccin")

	// If node is unreachable / offline, it must NEVER report "Alive"
	pillOffline := ui.GossipPill(entity.GossipStateAlive, false)
	if strings.Contains(pillOffline, "Alive") {
		t.Fatalf("critical bug: unreachable node with isOnline=false rendered as Alive: %s", pillOffline)
	}

	pillDead := ui.GossipPill(entity.GossipStateDead, false)
	if !strings.Contains(pillDead, "Dead") {
		t.Fatalf("expected dead badge, got: %s", pillDead)
	}

	// If node is online, Alive state is rendered properly
	pillOnline := ui.GossipPill(entity.GossipStateAlive, true)
	if !strings.Contains(pillOnline, "Alive") {
		t.Fatalf("expected alive badge for online node, got: %s", pillOnline)
	}
}

func TestRenderNodeTable_WideSupport(t *testing.T) {
	ui := NewUI("catppuccin")
	longAddr := "tcpGFwWCAAw5HcHmWwJc38SeFH1sYfAxnau6DyFasWpd__iZsfE2FrWCD1R6vKA4mHNhvjLE9AEwPfHI6noosUK4pqAjn-eH-eKWFxWCDFRzm5w0gFvleZc5LDOg7PcNDdr-xlT4Uc75WEjl27fmFygaFhToGjYWhudGMzMDJhLmlwbi5kZXZhNG0yMDguMTExLjM5LjM4YTZzMjYwNzpmNzQwOjA6M2Y6OjcyMA"

	nodes := []*entity.Node{
		{
			ID:          "node-1",
			Name:        "remote-box",
			Addr:        longAddr,
			IsOnline:    true,
			GossipState: entity.GossipStateAlive,
			LastSeen:    time.Now(),
		},
	}

	// 1. Default table: compact address, no MESH ADDR header
	defaultTable := ui.RenderNodeTable(nodes)
	if strings.Contains(defaultTable, "MESH ADDR") {
		t.Fatalf("default table should not contain MESH ADDR header")
	}
	if !strings.Contains(defaultTable, "tcpGFwWCAA...OjcyMA") {
		t.Fatalf("default table should display middle-truncated address in HOST / IP")
	}

	// 2. Wide table: includes full MESH ADDR column
	wideTable := ui.RenderNodeTable(nodes, true)
	if !strings.Contains(wideTable, "MESH ADDR") {
		t.Fatalf("wide table must contain MESH ADDR header")
	}
	if !strings.Contains(wideTable, longAddr) {
		t.Fatalf("wide table must contain full un-truncated address")
	}
}
