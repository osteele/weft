package terminal

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type keyHelpSection struct {
	Title string
	Lines []string
}

func renderKeyHelp(title string, sections []keyHelpSection, width int, height int, titleStyle lipgloss.Style, bodyStyle lipgloss.Style) string {
	if width <= 0 {
		width = 120
	}
	lines := keyHelpLines(title, sections)
	if shouldUseSingleColumnHelp(lines, width, height) {
		return renderSingleColumnKeyHelp(lines, width, titleStyle, bodyStyle)
	}
	return renderMultiColumnKeyHelp(lines, width, height, titleStyle, bodyStyle)
}

func shouldUseSingleColumnHelp(lines []string, width int, height int) bool {
	if width < 84 {
		return true
	}
	if height <= 0 {
		return true
	}
	return len(lines)+1 <= height
}

func keyHelpLines(title string, sections []keyHelpSection) []string {
	lines := []string{title}
	for _, section := range sections {
		lines = append(lines, "")
		if section.Title != "" {
			lines = append(lines, section.Title+":")
		}
		lines = append(lines, section.Lines...)
	}
	lines = append(lines, "", "Help:", "  ? toggle this help", "  q or Esc close help")
	return lines
}

func renderSingleColumnKeyHelp(lines []string, width int, titleStyle lipgloss.Style, bodyStyle lipgloss.Style) string {
	var b strings.Builder
	for i, line := range lines {
		style := bodyStyle
		if i == 0 {
			style = titleStyle
		}
		b.WriteString(style.Render(truncateDisplayWidth(line, width)))
		b.WriteString("\n")
	}
	b.WriteString(bodyStyle.Render(truncateDisplayWidth("? close help", width)))
	return b.String()
}

func renderMultiColumnKeyHelp(lines []string, width int, height int, titleStyle lipgloss.Style, bodyStyle lipgloss.Style) string {
	title := lines[0]
	body := lines[1:]
	cols := keyHelpColumnCount(len(body), width, height)
	gap := 3
	colWidth := (width - gap*(cols-1)) / cols
	rows := (len(body) + cols - 1) / cols
	var b strings.Builder
	b.WriteString(titleStyle.Render(truncateDisplayWidth(title, width)))
	b.WriteString("\n")
	for row := 0; row < rows; row++ {
		parts := make([]string, 0, cols)
		for col := 0; col < cols; col++ {
			idx := col*rows + row
			text := ""
			if idx < len(body) {
				text = truncateDisplayWidth(body[idx], colWidth)
			}
			if col < cols-1 {
				text = padRight(text, colWidth)
			}
			parts = append(parts, text)
		}
		line := strings.Join(parts, strings.Repeat(" ", gap))
		b.WriteString(bodyStyle.Render(truncateDisplayWidth(line, width)))
		b.WriteString("\n")
	}
	b.WriteString(bodyStyle.Render(truncateDisplayWidth("? close help", width)))
	return b.String()
}

func keyHelpColumnCount(bodyLines int, width int, height int) int {
	maxCols := 2
	if width >= 96 {
		maxCols = 3
	}
	if height <= 0 {
		return min(maxCols, 2)
	}
	availableRows := max(1, height-2)
	for cols := 2; cols <= maxCols; cols++ {
		if (bodyLines+cols-1)/cols <= availableRows {
			return cols
		}
	}
	return maxCols
}
