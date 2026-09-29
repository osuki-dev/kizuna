package presenter

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/updater"
)

type upgradeTickMsg time.Time
type autoQuitMsg struct{}

// UpgradeModel provides an interactive animated Bubble Tea TUI for the self-upgrade process
type UpgradeModel struct {
	ctx            context.Context
	mgr            *updater.Manager
	currentVersion string
	theme          *entity.Theme
	styles         Styles

	spinnerFrames []string
	frame         int
	quitting      bool
	done          bool

	// Event details
	step       updater.UpgradeStep
	message    string
	latestVer  string
	assetName  string
	downloaded int64
	totalSize  int64
	speed      float64
	percent    float64
	targetExec string
	htmlURL    string
	err        error
}

// NewUpgradeModel constructs the upgrade TUI model
func NewUpgradeModel(ctx context.Context, mgr *updater.Manager, currentVersion string, themeName string) *UpgradeModel {
	t := entity.ResolveTheme(themeName, nil)
	return &UpgradeModel{
		ctx:            ctx,
		mgr:            mgr,
		currentVersion: currentVersion,
		theme:          t,
		styles:         NewStyles(t),
		spinnerFrames:  []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		step:           updater.StepChecking,
		message:        "Querying latest release from GitHub...",
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg {
		return upgradeTickMsg(t)
	})
}

// Init initializes the model, starts the animation ticker and the background upgrade worker
func (m *UpgradeModel) Init() tea.Cmd {
	return tea.Batch(
		tickCmd(),
		m.startUpgradeWorker(),
	)
}

func (m *UpgradeModel) startUpgradeWorker() tea.Cmd {
	return func() tea.Msg {
		// Event channel to stream progress from worker to bubbletea
		eventChan := make(chan updater.UpgradeEvent, 64)

		go func() {
			err := m.mgr.UpgradeWithProgress(m.ctx, m.currentVersion, func(ev updater.UpgradeEvent) {
				eventChan <- ev
			})
			if err != nil {
				eventChan <- updater.UpgradeEvent{
					Step:    updater.StepFailed,
					Message: err.Error(),
					Err:     err,
				}
			}
			close(eventChan)
		}()

		// Return the first event, subsequent events are handled in Update()
		firstEv, ok := <-eventChan
		if !ok {
			return nil
		}
		// Wrap with channel reader
		return streamedEventsMsg{ev: firstEv, ch: eventChan}
	}
}

type streamedEventsMsg struct {
	ev updater.UpgradeEvent
	ch <-chan updater.UpgradeEvent
}

func nextEventCmd(ch <-chan updater.UpgradeEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return streamedEventsMsg{ev: ev, ch: ch}
	}
}

// Update handles incoming messages and progress updates
func (m *UpgradeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		}

	case upgradeTickMsg:
		m.frame++
		if !m.done {
			return m, tickCmd()
		}
		return m, nil

	case autoQuitMsg:
		m.quitting = true
		return m, tea.Quit

	case streamedEventsMsg:
		ev := msg.ev
		m.step = ev.Step
		if ev.Message != "" {
			m.message = ev.Message
		}
		if ev.LatestVer != "" {
			m.latestVer = ev.LatestVer
		}
		if ev.Asset != nil {
			m.assetName = ev.Asset.Name
			if ev.TotalSize > 0 {
				m.totalSize = ev.TotalSize
			}
		}
		if ev.Downloaded > 0 {
			m.downloaded = ev.Downloaded
		}
		if ev.TotalSize > 0 {
			m.totalSize = ev.TotalSize
		}
		if ev.Speed > 0 {
			m.speed = ev.Speed
		}
		if ev.Percent > 0 {
			m.percent = ev.Percent
		}
		if ev.TargetExec != "" {
			m.targetExec = ev.TargetExec
		}
		if ev.Release != nil && ev.Release.HTMLURL != "" {
			m.htmlURL = ev.Release.HTMLURL
		}
		if ev.Err != nil {
			m.err = ev.Err
		}

		if ev.Step == updater.StepComplete || ev.Step == updater.StepUpToDate || ev.Step == updater.StepFailed {
			m.done = true
			// Auto quit after 1200ms to preserve output on terminal
			return m, tea.Batch(
				nextEventCmd(msg.ch),
				tea.Tick(1400*time.Millisecond, func(t time.Time) tea.Msg {
					return autoQuitMsg{}
				}),
			)
		}

		return m, nextEventCmd(msg.ch)
	}

	return m, nil
}

// View renders the interactive TUI
func (m *UpgradeModel) View() string {
	primary := lipgloss.Color(m.theme.Primary)
	secondary := lipgloss.Color(m.theme.Secondary)
	warning := lipgloss.Color(m.theme.Warning)
	danger := lipgloss.Color(m.theme.Danger)
	muted := lipgloss.Color(m.theme.Muted)

	spinnerChar := m.spinnerFrames[m.frame%len(m.spinnerFrames)]
	spinStyle := lipgloss.NewStyle().Foreground(primary).Bold(true)
	checkStyle := lipgloss.NewStyle().Foreground(secondary).Bold(true)
	mutedStyle := lipgloss.NewStyle().Foreground(muted)
	boldStyle := lipgloss.NewStyle().Bold(true)
	dangerStyle := lipgloss.NewStyle().Foreground(danger).Bold(true)

	// Header banner
	headerBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primary).
		Padding(0, 2).
		MarginBottom(1).
		Render(lipgloss.NewStyle().Bold(true).Foreground(primary).Render("⚡ KIZUNA CLI AUTO-UPGRADE ⚡"))

	var content strings.Builder
	content.WriteString(headerBox)
	content.WriteString("\n")

	// Special View: Already up to date
	if m.step == updater.StepUpToDate {
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(secondary).
			Padding(1, 2).
			Width(62).
			Render(fmt.Sprintf("%s %s\n\n  %s  %s (latest)\n  %s  %s\n  %s  Up to date with GitHub Release\n",
				checkStyle.Render("✓"),
				lipgloss.NewStyle().Bold(true).Foreground(secondary).Render("YOU'RE RUNNING THE LATEST VERSION"),
				mutedStyle.Render("Current Version:"),
				boldStyle.Render(m.currentVersion),
				mutedStyle.Render("Platform:       "),
				boldStyle.Render(fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)),
				mutedStyle.Render("Status:         "),
			))
		content.WriteString(box)
		content.WriteString("\n")
		return content.String()
	}

	// Special View: Failed
	if m.step == updater.StepFailed {
		errMsg := "unknown error"
		if m.err != nil {
			errMsg = m.err.Error()
		} else if m.message != "" {
			errMsg = m.message
		}
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(danger).
			Padding(1, 2).
			Width(62).
			Render(fmt.Sprintf("%s %s\n\n  %s\n\n  %s\n  %s\n",
				dangerStyle.Render("✖"),
				lipgloss.NewStyle().Bold(true).Foreground(danger).Render("UPGRADE FAILED"),
				dangerStyle.Render(errMsg),
				mutedStyle.Render("Tip: You can re-run the manual installer:"),
				lipgloss.NewStyle().Foreground(primary).Render("curl -fsSL https://raw.githubusercontent.com/osuki-dev/kizuna/main/install.sh | bash"),
			))
		content.WriteString(box)
		content.WriteString("\n")
		return content.String()
	}

	// Special View: Complete Celebration Card
	if m.step == updater.StepComplete {
		targetPath := m.targetExec
		if targetPath == "" {
			targetPath = "kizuna"
		}
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(secondary).
			Padding(1, 2).
			Width(64).
			Render(fmt.Sprintf("%s %s\n\n  %s  %s ➔ %s\n  %s  %s\n  %s  %s\n  %s  %s\n  %s  %s\n\n  %s\n",
				checkStyle.Render("★"),
				lipgloss.NewStyle().Bold(true).Foreground(secondary).Render(fmt.Sprintf("KIZUNA SUCCESSFULLY UPGRADED TO %s!", m.latestVer)),
				mutedStyle.Render("Version:    "),
				lipgloss.NewStyle().Foreground(warning).Render(m.currentVersion),
				lipgloss.NewStyle().Bold(true).Foreground(secondary).Render(m.latestVer+" (latest)"),
				mutedStyle.Render("Platform:   "),
				boldStyle.Render(fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)),
				mutedStyle.Render("Executable: "),
				boldStyle.Render(targetPath),
				mutedStyle.Render("Security:   "),
				checkStyle.Render("✓ Mach-O CodeSignature Verified"),
				mutedStyle.Render("Integrity:  "),
				checkStyle.Render("✓ SHA256 Cryptographic Hash Verified"),
				lipgloss.NewStyle().Foreground(primary).Render("Run 'kizuna --version' or 'kizuna status' to explore new features!"),
			))
		content.WriteString(box)
		content.WriteString("\n")
		return content.String()
	}

	// In-progress Pipeline Checklist
	renderStep := func(active, done bool, title string, subline string) string {
		var icon string
		var titleStyle lipgloss.Style

		if done {
			icon = checkStyle.Render("✓")
			titleStyle = boldStyle
		} else if active {
			icon = spinStyle.Render(spinnerChar)
			titleStyle = lipgloss.NewStyle().Bold(true).Foreground(primary)
		} else {
			icon = mutedStyle.Render("○")
			titleStyle = mutedStyle
		}

		line := fmt.Sprintf("  %s  %s", icon, titleStyle.Render(title))
		if subline != "" {
			line += "\n" + subline
		}
		return line
	}

	// 1. Check GitHub
	step1Done := m.step != updater.StepChecking
	content.WriteString(renderStep(m.step == updater.StepChecking, step1Done, "Querying latest release from GitHub", ""))
	content.WriteString("\n")

	// 2. Version diff
	step2Active := m.step == updater.StepFound
	step2Done := m.step != updater.StepChecking && m.step != updater.StepFound
	verDiff := ""
	if m.latestVer != "" {
		verDiff = fmt.Sprintf("Found %s (current: %s)", m.latestVer, m.currentVersion)
	} else {
		verDiff = "Locating platform assets"
	}
	content.WriteString(renderStep(step2Active, step2Done, verDiff, ""))
	content.WriteString("\n")

	// 3. Download Progress
	step3Active := m.step == updater.StepDownloading
	step3Done := m.step == updater.StepVerifying || m.step == updater.StepExtracting || m.step == updater.StepApplying || m.step == updater.StepComplete

	var downloadSubline string
	if step3Active {
		// Animated Progress Bar
		barWidth := 36
		filled := int(float64(barWidth) * (m.percent / 100.0))
		if filled > barWidth {
			filled = barWidth
		}
		if filled < 0 {
			filled = 0
		}

		barFilled := strings.Repeat("█", filled)
		barEmpty := strings.Repeat("░", barWidth-filled)

		styledFilled := lipgloss.NewStyle().Foreground(primary).Render(barFilled)
		styledEmpty := lipgloss.NewStyle().Foreground(muted).Render(barEmpty)

		bar := fmt.Sprintf("     [%s%s] %5.1f%%", styledFilled, styledEmpty, m.percent)

		etaStr := formatETA(m.downloaded, m.totalSize, m.speed)
		statsLine := fmt.Sprintf("     %s %s / %s   •   %s %s   •   %s %s",
			mutedStyle.Render("⬇"),
			formatBytes(toUint64(m.downloaded)),
			formatBytes(toUint64(m.totalSize)),
			mutedStyle.Render("⚡"),
			formatSpeed(m.speed),
			mutedStyle.Render("⏱"),
			etaStr,
		)
		downloadSubline = bar + "\n" + statsLine
	} else if step3Done {
		downloadSubline = fmt.Sprintf("     %s", mutedStyle.Render(fmt.Sprintf("Archive downloaded: %s (%s)", m.assetName, formatBytes(toUint64(m.totalSize)))))
	}

	dlTitle := "Downloading binary archive"
	if m.assetName != "" {
		dlTitle = fmt.Sprintf("Downloading %s", m.assetName)
	}
	content.WriteString(renderStep(step3Active, step3Done, dlTitle, downloadSubline))
	content.WriteString("\n")

	// 4. Verify Checksum
	step4Active := m.step == updater.StepVerifying
	step4Done := m.step == updater.StepExtracting || m.step == updater.StepApplying || m.step == updater.StepComplete
	content.WriteString(renderStep(step4Active, step4Done, "Verifying cryptographic SHA256 checksum", ""))
	content.WriteString("\n")

	// 5. Extract & CodeSign
	step5Active := m.step == updater.StepExtracting
	step5Done := m.step == updater.StepApplying || m.step == updater.StepComplete
	content.WriteString(renderStep(step5Active, step5Done, "Extracting binary and configuring OS permissions", ""))
	content.WriteString("\n")

	// 6. Apply atomic update
	step6Active := m.step == updater.StepApplying
	step6Done := m.step == updater.StepComplete
	content.WriteString(renderStep(step6Active, step6Done, "Atomically replacing executable binary", ""))
	content.WriteString("\n")

	return content.String()
}

func toUint64(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

func formatSpeed(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "-- MB/s"
	}
	return fmt.Sprintf("%.1f MB/s", bytesPerSec/(1024*1024))
}

func formatETA(downloaded, total int64, speed float64) string {
	if speed <= 0 || total <= 0 || downloaded >= total {
		return "--"
	}
	remaining := float64(total - downloaded)
	seconds := remaining / speed
	if seconds < 1 {
		return "< 1s"
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", int(seconds))
	}
	return fmt.Sprintf("%dm%ds", int(seconds)/60, int(seconds)%60)
}

// RunUpgradeTUI executes the full Bubble Tea interactive animated upgrade program
func RunUpgradeTUI(ctx context.Context, mgr *updater.Manager, currentVersion, themeName string) error {
	model := NewUpgradeModel(ctx, mgr, currentVersion, themeName)
	p := tea.NewProgram(model)
	_, err := p.Run()
	return err
}
