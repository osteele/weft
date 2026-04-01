package flash

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// State tracks an auto-dismissing flash message for terminal UIs.
type State struct {
	Message string
	IsError bool
	Expiry  time.Time
}

// ExpiredMsg is sent when a flash message reaches its display deadline.
type ExpiredMsg struct{}

const duration = 3 * time.Second

var (
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("124")).Bold(true).Padding(0, 1)
	normalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("240")).Padding(0, 1)
)

// Set shows a flash message and schedules its expiration.
func (f *State) Set(msg string, isError bool) tea.Cmd {
	f.Message = msg
	f.IsError = isError
	f.Expiry = time.Now().Add(duration)
	return tea.Tick(duration, func(time.Time) tea.Msg { return ExpiredMsg{} })
}

// Clear removes the current flash message.
func (f *State) Clear() {
	f.Message = ""
	f.IsError = false
	f.Expiry = time.Time{}
}

// HandleExpired clears the flash if its expiry has passed.
func (f *State) HandleExpired() {
	if !f.Expiry.IsZero() && !time.Now().Before(f.Expiry) {
		f.Clear()
	}
}

// Render returns the styled flash text.
func (f *State) Render() string {
	if f.Message == "" {
		return ""
	}
	if f.IsError {
		return " " + errorStyle.Render(f.Message)
	}
	return " " + normalStyle.Render(f.Message)
}
