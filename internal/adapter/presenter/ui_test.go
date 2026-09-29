package presenter_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/adapter/presenter"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

func TestUIBannerAndBadges(t *testing.T) {
	ui := presenter.NewUI("tokyonight")

	banner := ui.Banner()
	if !strings.Contains(banner, "KIZUNA") {
		t.Errorf("expected banner to contain KIZUNA")
	}

	badge := ui.Badge("TEST", "#FFF", "#000")
	if badge == "" {
		t.Errorf("expected non-empty badge")
	}

	envBadge := ui.EnvBadge("staging")
	if !strings.Contains(envBadge, "STAGING") {
		t.Errorf("expected envBadge to contain STAGING")
	}
}

func TestRenderTables(t *testing.T) {
	ui := presenter.NewUI("catppuccin")

	nodes := []*entity.Node{
		{
			ID:       "node_1",
			Name:     "home-server",
			IsOnline: true,
			OS:       "linux",
			Arch:     "amd64",
			Tags:     []string{"home", "prod"},
			Host:     "10.0.0.2",
			LastSeen: time.Now(),
		},
	}

	tbl := ui.RenderNodeTable(nodes)
	if !strings.Contains(tbl, "home-server") {
		t.Errorf("expected node table to contain home-server")
	}
	if !strings.Contains(tbl, "home, prod") {
		t.Errorf("expected node table to contain tags 'home, prod'")
	}
	if !strings.Contains(tbl, "10.0.0.2") {
		t.Errorf("expected node table to contain host '10.0.0.2'")
	}

	services := []*entity.Service{
		{
			Name:  "web",
			Type:  entity.TypeDocker,
			State: entity.StateRunning,
			Ports: []string{"3000:3000"},
		},
	}

	sTbl := ui.RenderServicesTable(services)
	if !strings.Contains(sTbl, "web") {
		t.Errorf("expected services table to contain web")
	}
}

func TestPrintJSON(t *testing.T) {
	buf := &bytes.Buffer{}
	data := map[string]string{"status": "ok"}
	if err := presenter.PrintJSON(buf, data); err != nil {
		t.Fatalf("PrintJSON failed: %v", err)
	}
	if !strings.Contains(buf.String(), `"status": "ok"`) {
		t.Errorf("expected JSON to contain key/value")
	}
}
