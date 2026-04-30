package banner

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	infoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("33")). // blue
			Bold(true)
	warningStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")). // orange
			Bold(true)
	criticalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")). // red
			Bold(true)
	hintStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")) // dim gray
)

// Render returns a multi-line string containing one styled line per banner,
// or "" if banners is empty. Each line begins with a severity sigil
// (i / ! / !!) so output stays legible in non-color terminals.
func Render(banners []Banner) string {
	if len(banners) == 0 {
		return ""
	}
	var b strings.Builder
	for i, ban := range banners {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(renderOne(ban))
	}
	return b.String()
}

func renderOne(ban Banner) string {
	var (
		sigil string
		style lipgloss.Style
	)
	switch ban.Severity {
	case SeverityCritical:
		sigil, style = "!!", criticalStyle
	case SeverityWarning:
		sigil, style = "! ", warningStyle
	default:
		sigil, style = "i ", infoStyle
	}
	main := style.Render(sigil + " " + ban.Text)
	if ban.Hint == "" {
		return main
	}
	return main + " " + hintStyle.Render("("+ban.Hint+")")
}
