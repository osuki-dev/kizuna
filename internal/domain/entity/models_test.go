package entity_test

import (
	"testing"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

func TestParseTargetHost(t *testing.T) {
	tests := []struct {
		input        string
		expectedType entity.TargetType
		expectedHost string
		expectedPort int
		expectedUser string
		expectedAddr string
	}{
		{
			input:        "localhost",
			expectedType: entity.TargetTypeLocal,
			expectedHost: "127.0.0.1",
			expectedPort: 0,
			expectedAddr: "127.0.0.1",
		},
		{
			input:        "ssh://deploy@192.168.1.100:2222",
			expectedType: entity.TargetTypeSSH,
			expectedHost: "192.168.1.100",
			expectedPort: 2222,
			expectedUser: "deploy",
			expectedAddr: "192.168.1.100:2222",
		},
		{
			input:        "root@10.0.0.5",
			expectedType: entity.TargetTypeSSH,
			expectedHost: "10.0.0.5",
			expectedPort: 22,
			expectedUser: "root",
			expectedAddr: "10.0.0.5",
		},
		{
			input:        "172.16.0.10",
			expectedType: entity.TargetTypeSSH,
			expectedHost: "172.16.0.10",
			expectedPort: 22,
			expectedUser: "root",
			expectedAddr: "172.16.0.10",
		},
		{
			input:        "my-mesh-worker-node",
			expectedType: entity.TargetTypeMesh,
			expectedHost: "my-mesh-worker-node",
			expectedAddr: "my-mesh-worker-node",
		},
	}

	for _, tc := range tests {
		th := entity.ParseTargetHost(tc.input)
		if th.Type != tc.expectedType {
			t.Errorf("input %q: expected type %s, got %s", tc.input, tc.expectedType, th.Type)
		}
		if th.Host != tc.expectedHost {
			t.Errorf("input %q: expected host %s, got %s", tc.input, tc.expectedHost, th.Host)
		}
		if tc.expectedPort > 0 && th.Port != tc.expectedPort {
			t.Errorf("input %q: expected port %d, got %d", tc.input, tc.expectedPort, th.Port)
		}
		if tc.expectedUser != "" && th.User != tc.expectedUser {
			t.Errorf("input %q: expected user %s, got %s", tc.input, tc.expectedUser, th.User)
		}
		if th.Address() != tc.expectedAddr {
			t.Errorf("input %q: expected address %s, got %s", tc.input, tc.expectedAddr, th.Address())
		}
	}
}

func TestResolveTheme(t *testing.T) {
	// 1. Default theme when empty or unknown
	def := entity.ResolveTheme("", nil)
	if def.Name != "Catppuccin Mocha" {
		t.Errorf("expected default Catppuccin Mocha, got %s", def.Name)
	}

	unk := entity.ResolveTheme("unknown-theme", nil)
	if unk.Name != "Catppuccin Mocha" {
		t.Errorf("expected fallback Catppuccin Mocha, got %s", unk.Name)
	}

	// 2. Built-in presets
	presets := []string{"catppuccin", "tokyonight", "dracula", "nord"}
	for _, p := range presets {
		theme := entity.ResolveTheme(p, nil)
		if theme.Primary == "" || theme.Secondary == "" || theme.Danger == "" {
			t.Errorf("theme %s missing primary colors", p)
		}
	}

	// 3. Custom theme override with defaults
	custom := &entity.Theme{
		Name:    "Custom",
		Primary: "#123456",
	}
	resolved := entity.ResolveTheme("", custom)
	if resolved.Primary != "#123456" {
		t.Errorf("expected custom primary #123456, got %s", resolved.Primary)
	}
	if resolved.Secondary != "#04B575" {
		t.Errorf("expected filled default secondary, got %s", resolved.Secondary)
	}
	if resolved.BoxBorder != "#123456" {
		t.Errorf("expected box border matching primary, got %s", resolved.BoxBorder)
	}
}
