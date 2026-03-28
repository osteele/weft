package cmd

import "github.com/charmbracelet/lipgloss"

// Shared TUI color palette used across cmd/ TUI files.
// Individual TUIs can compose these into more specific styles.
var (
	tuiAccentColor    = lipgloss.Color("39")  // cyan — headers, cursors, status
	tuiRunningColor   = lipgloss.Color("10")  // green — active/running/selected
	tuiCompletedColor = lipgloss.Color("243") // gray — completed, dim, terminal
	tuiFailedColor    = lipgloss.Color("196") // red — errors, failures
	tuiSelectedBg     = lipgloss.Color("240") // dark gray — row highlight
)

// Shared styles composed from the palette above.
var (
	tuiTitleStyle       = lipgloss.NewStyle().Bold(true)
	tuiAccentStyle      = lipgloss.NewStyle().Foreground(tuiAccentColor)
	tuiRunningStyle     = lipgloss.NewStyle().Foreground(tuiRunningColor)
	tuiDimStyle         = lipgloss.NewStyle().Foreground(tuiCompletedColor)
	tuiFailedStyle      = lipgloss.NewStyle().Foreground(tuiFailedColor)
	tuiSelectedRowStyle = lipgloss.NewStyle().Background(tuiSelectedBg)
	tuiCursorStyle      = lipgloss.NewStyle().Foreground(tuiAccentColor).Bold(true)
)
