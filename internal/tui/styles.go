package tui

import "github.com/charmbracelet/lipgloss"

var (
	// Colors
	runningColor   = lipgloss.Color("10") // Green
	completedColor = lipgloss.Color("8")  // Gray
	failedColor    = lipgloss.Color("9")  // Red
	deadColor      = lipgloss.Color("9")  // Red
	pendingColor   = lipgloss.Color("11") // Yellow
	queuedColor    = lipgloss.Color("6")  // Cyan
	selectedBg     = lipgloss.Color("4")  // Blue
	borderColor    = lipgloss.Color("8")  // Gray

	// Panel styles
	listPanelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderColor).
			Padding(0, 1)

	logPanelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderColor).
			Padding(0, 1)

	// Selection style
	selectedStyle = lipgloss.NewStyle().
			Background(selectedBg).
			Foreground(lipgloss.Color("15")).
			Bold(true)

	// Status-based styles
	runningStyle = lipgloss.NewStyle().
			Foreground(runningColor)

	completedStyle = lipgloss.NewStyle().
			Foreground(completedColor)

	failedStyle = lipgloss.NewStyle().
			Foreground(failedColor)

	deadStyle = lipgloss.NewStyle().
			Foreground(deadColor)

	pendingStyle = lipgloss.NewStyle().
			Foreground(pendingColor)

	queuedStyle = lipgloss.NewStyle().
			Foreground(queuedColor)

	// Text styles
	headerStyle = lipgloss.NewStyle().
			Bold(true)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Padding(0, 1)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9")).
			Bold(true)

	statusMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	syncingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11"))

	// Host status styles
	hostOnlineStyle = lipgloss.NewStyle().
			Foreground(runningColor) // Green

	hostOfflineStyle = lipgloss.NewStyle().
				Foreground(failedColor) // Red

	hostCheckingStyle = lipgloss.NewStyle().
				Foreground(pendingColor) // Yellow
)
