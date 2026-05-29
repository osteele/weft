package dashtabs

import "github.com/charmbracelet/lipgloss"

// Palette mirrors internal/ui/terminal/tui_styles.go so the dashboard reads
// as part of the same product. We duplicate rather than import to avoid
// pulling the whole terminal package's transitive deps into this leaf TUI.
var (
	colorAccent    = lipgloss.Color("39")  // cyan — headers, cursors, active tab
	colorRunning   = lipgloss.Color("10")  // green — running, OK
	colorQueued    = lipgloss.Color("214") // orange — queued, attention
	colorCompleted = lipgloss.Color("243") // gray — completed, dim
	colorFailed    = lipgloss.Color("196") // red — errors, failures
	colorDim       = lipgloss.Color("242") // dim gray — secondary
	colorSelectBg  = lipgloss.Color("240") // row highlight
	colorPaused    = lipgloss.Color("178") // amber — paused
)

var (
	titleStyle     = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	dimStyle       = lipgloss.NewStyle().Foreground(colorDim)
	runningStyle   = lipgloss.NewStyle().Foreground(colorRunning)
	queuedStyle    = lipgloss.NewStyle().Foreground(colorQueued)
	completedStyle = lipgloss.NewStyle().Foreground(colorCompleted)
	failedStyle    = lipgloss.NewStyle().Foreground(colorFailed)
	pausedStyle    = lipgloss.NewStyle().Foreground(colorPaused).Bold(true)
	accentStyle    = lipgloss.NewStyle().Foreground(colorAccent)

	tabActiveStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colorAccent).Padding(0, 1)
	tabInactiveStyle = lipgloss.NewStyle().Foreground(colorDim).Padding(0, 1)
	tabAttnStyle     = lipgloss.NewStyle().Foreground(colorFailed).Padding(0, 1)

	helpBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorAccent).
			Padding(1, 2)

	statusLineStyle = lipgloss.NewStyle().Foreground(colorDim)
)
