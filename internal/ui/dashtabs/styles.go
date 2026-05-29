package dashtabs

import "github.com/charmbracelet/lipgloss"

// Palette chosen to read well on light terminal backgrounds: every coloured
// text token uses a darker, less saturated variant than the standard ANSI
// brights. Each comment notes the 6×6×6 RGB cube coordinate (R G B in 0..5)
// so future adjustments stay perceptually aligned.
var (
	colorAccent    = lipgloss.Color("25")  // darker cyan/blue — R0 G2 B4
	colorRunning   = lipgloss.Color("28")  // darker green — R0 G3 B0
	colorQueued    = lipgloss.Color("166") // burnt orange — R4 G2 B0
	colorCompleted = lipgloss.Color("243") // medium gray — slightly darker than 245
	colorFailed    = lipgloss.Color("124") // darker red — R3 G0 B0
	colorDim       = lipgloss.Color("242") // dim gray — secondary
	colorSelectBg  = lipgloss.Color("238") // darker row highlight bg
	colorPaused    = lipgloss.Color("136") // burnt amber — R3 G2 B0
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
