package cmd

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type flashState struct {
	message string
	isError bool
	expiry  time.Time
}

type flashExpiredMsg struct{}

const flashDuration = 3 * time.Second

var (
	flashErrorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Padding(0, 1).Background(tuiFailedColor)
	flashNormalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Padding(0, 1).Background(tuiSelectedBg)
)

// set displays a flash message that auto-dismisses after flashDuration.
func (f *flashState) set(msg string, isError bool) tea.Cmd {
	f.message = msg
	f.isError = isError
	f.expiry = time.Now().Add(flashDuration)
	return tea.Tick(flashDuration, func(time.Time) tea.Msg { return flashExpiredMsg{} })
}

// handleExpired clears the flash if the expiry has passed.
// Call from Update() when receiving flashExpiredMsg.
func (f *flashState) handleExpired() {
	if !f.expiry.IsZero() && !time.Now().Before(f.expiry) {
		f.message = ""
	}
}

func (f *flashState) render() string {
	if f.message == "" {
		return ""
	}
	if f.isError {
		return flashErrorStyle.Render(f.message)
	}
	return flashNormalStyle.Render(f.message)
}
