package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// FlashState provides auto-dismissing flash messages for Bubble Tea models.
// Embed in a model struct and call Set/HandleExpired/Render as needed.
type FlashState struct {
	Message string
	IsError bool
	Expiry  time.Time
}

// FlashExpiredMsg is sent when a flash message's display duration elapses.
type FlashExpiredMsg struct{}

const flashDuration = 3 * time.Second

var (
	flashErrorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("124")).Bold(true).Padding(0, 1)
	flashNormalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("240")).Padding(0, 1)
)

// Set displays a flash message that auto-dismisses after flashDuration.
func (f *FlashState) Set(msg string, isError bool) tea.Cmd {
	f.Message = msg
	f.IsError = isError
	f.Expiry = time.Now().Add(flashDuration)
	return tea.Tick(flashDuration, func(time.Time) tea.Msg { return FlashExpiredMsg{} })
}

// Clear dismisses the flash immediately.
func (f *FlashState) Clear() {
	f.Message = ""
	f.IsError = false
	f.Expiry = time.Time{}
}

// HandleExpired clears the flash if the expiry has passed.
// Call from Update() when receiving FlashExpiredMsg.
func (f *FlashState) HandleExpired() {
	if !f.Expiry.IsZero() && !time.Now().Before(f.Expiry) {
		f.Message = ""
		f.IsError = false
		f.Expiry = time.Time{}
	}
}

// Render returns the styled flash message, or empty string if none.
func (f *FlashState) Render() string {
	if f.Message == "" {
		return ""
	}
	if f.IsError {
		return " " + flashErrorStyle.Render(f.Message)
	}
	return " " + flashNormalStyle.Render(f.Message)
}
