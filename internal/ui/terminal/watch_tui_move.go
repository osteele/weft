package terminal

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	dashboard "github.com/osteele/weft/internal/ui/dashboard"
)

// moveOption represents a destination for moving a queued job.
type moveOption struct {
	isNew       bool                      // true = launch new instance; false = move to existing
	instanceID  int64                     // DB launch ID (existing only)
	offer       *cloud.Offer              // cloud offer (new only)
	strategy    bidding.SelectionStrategy // "cheap"/"fast"/"fastest" (new only)
	waitTime    time.Duration             // estimated wait (existing) or setup overhead (new)
	costPerHour float64                   // $/hr
	gpuName     string                    // resolved GPU name for display
}

// movePickerModel is an inline overlay for selecting a move destination.
type movePickerModel struct {
	active  bool
	jobID   int64
	options []moveOption
	cursor  int
}

func (p *movePickerModel) reset() {
	p.active = false
	p.jobID = 0
	p.options = nil
	p.cursor = 0
}

func (p *movePickerModel) selectableCount() int {
	return len(p.options)
}

func (p *movePickerModel) moveCursor(delta int) {
	n := p.selectableCount()
	if n == 0 {
		return
	}
	p.cursor += delta
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= n {
		p.cursor = n - 1
	}
}

func (p *movePickerModel) selectedOption() *moveOption {
	if p.cursor >= 0 && p.cursor < len(p.options) {
		return &p.options[p.cursor]
	}
	return nil
}

var (
	movePickerBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(tuiAccentColor).
				Padding(1, 2)
	movePickerTitleStyle    = tuiTitleStyle
	movePickerSectionStyle  = tuiDimStyle
	movePickerSelectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("62")).Foreground(lipgloss.Color("230"))
	movePickerDimStyle      = tuiDimStyle
)

// View renders the move picker overlay.
func (p *movePickerModel) View(width, height int) string {
	if !p.active || len(p.options) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(movePickerTitleStyle.Render(fmt.Sprintf("Move job #%d to:", p.jobID)))
	b.WriteString("\n")

	// Separate existing vs new options
	hasExisting := false
	hasNew := false
	for _, o := range p.options {
		if o.isNew {
			hasNew = true
		} else {
			hasExisting = true
		}
	}

	if hasExisting {
		b.WriteString("\n")
		b.WriteString(movePickerSectionStyle.Render("EXISTING INSTANCES"))
		b.WriteString("\n")
		for i, o := range p.options {
			if o.isNew {
				continue
			}
			line := formatMoveOptionLine(o)
			if i == p.cursor {
				line = movePickerSelectedStyle.Render(line)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	if hasNew {
		b.WriteString("\n")
		b.WriteString(movePickerSectionStyle.Render("NEW INSTANCE"))
		b.WriteString("\n")
		for i, o := range p.options {
			if !o.isNew {
				continue
			}
			line := formatMoveOptionLine(o)
			if i == p.cursor {
				line = movePickerSelectedStyle.Render(line)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(movePickerDimStyle.Render("[↑↓] select  [enter] confirm  [esc] cancel"))

	content := movePickerBorderStyle.Render(b.String())

	// Center the overlay
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func formatMoveOptionLine(o moveOption) string {
	if o.isNew {
		return fmt.Sprintf("  %-10s %-14s ~%s setup  $%.2f/hr",
			string(o.strategy)+":", o.gpuName, dashboard.FormatCompactDuration(o.waitTime), o.costPerHour)
	}
	return fmt.Sprintf("  Instance #%-4d %-14s ~%s wait  $%.2f/hr",
		o.instanceID, o.gpuName, dashboard.FormatCompactDuration(o.waitTime), o.costPerHour)
}
