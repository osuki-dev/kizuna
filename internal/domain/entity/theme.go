package entity

import "strings"

// Theme represents the visual styling colors for the TUI Dashboard
type Theme struct {
	Name       string `yaml:"name" json:"name"`
	Primary    string `yaml:"primary" json:"primary"`
	Secondary  string `yaml:"secondary" json:"secondary"`
	Danger     string `yaml:"danger" json:"danger"`
	Warning    string `yaml:"warning" json:"warning"`
	Text       string `yaml:"text" json:"text"`
	Muted      string `yaml:"muted" json:"muted"`
	Background string `yaml:"background" json:"background"`
	BoxBorder  string `yaml:"box_border" json:"box_border"`
	Selection  string `yaml:"selection,omitempty" json:"selection,omitempty"`
}

// Built-in theme presets
var themePresets = map[string]*Theme{
	"catppuccin": {
		Name:       "Catppuccin Mocha",
		Primary:    "#CBA6F7", // Mauve
		Secondary:  "#A6E3A1", // Green
		Danger:     "#F38BA8", // Red
		Warning:    "#F9E2AF", // Yellow
		Text:       "#CDD6F4", // Text
		Muted:      "#6C7086", // Overlay0
		Background: "#1E1E2E", // Base
		BoxBorder:  "#CBA6F7",
		Selection:  "#313244", // Surface0
	},
	"tokyonight": {
		Name:       "Tokyo Night",
		Primary:    "#7AA2F7", // Blue
		Secondary:  "#9ECE6A", // Green
		Danger:     "#F7768E", // Red
		Warning:    "#E0AF68", // Yellow
		Text:       "#C0CAF5",
		Muted:      "#565F89",
		Background: "#1A1B26",
		BoxBorder:  "#7AA2F7",
		Selection:  "#292E42",
	},
	"dracula": {
		Name:       "Dracula",
		Primary:    "#BD93F9", // Purple
		Secondary:  "#50FA7B", // Green
		Danger:     "#FF5555", // Red
		Warning:    "#F1FA8C", // Yellow
		Text:       "#F8F8F2",
		Muted:      "#6272A4",
		Background: "#282A36",
		BoxBorder:  "#BD93F9",
		Selection:  "#44475A",
	},
	"nord": {
		Name:       "Nord",
		Primary:    "#88C0D0", // Frost Cyan
		Secondary:  "#A3BE8C", // Aurora Green
		Danger:     "#BF616A", // Aurora Red
		Warning:    "#EBCB8B", // Aurora Yellow
		Text:       "#ECEFF4", // Snow Storm
		Muted:      "#4C566A", // Polar Night
		Background: "#2E3440",
		BoxBorder:  "#88C0D0",
		Selection:  "#3B4252",
	},
}

// DefaultTheme returns the default Catppuccin theme
func DefaultTheme() *Theme {
	return themePresets["catppuccin"]
}

// ResolveTheme returns a theme preset by name or custom theme
func ResolveTheme(name string, custom *Theme) *Theme {
	if custom != nil && custom.Primary != "" {
		if custom.Secondary == "" {
			custom.Secondary = "#04B575"
		}
		if custom.Danger == "" {
			custom.Danger = "#FF4757"
		}
		if custom.Text == "" {
			custom.Text = "#FAFAFA"
		}
		if custom.BoxBorder == "" {
			custom.BoxBorder = custom.Primary
		}
		if custom.Selection == "" {
			custom.Selection = "#313244"
		}
		return custom
	}

	nameLower := strings.ToLower(strings.TrimSpace(name))
	if preset, ok := themePresets[nameLower]; ok {
		return preset
	}
	return DefaultTheme()
}
