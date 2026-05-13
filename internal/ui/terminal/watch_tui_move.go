package terminal

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
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
	active       bool
	jobID        int64
	requestID    int64
	options      []moveOption
	cursor       int
	loadingNew   bool
	status       string
	existingDone bool
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

func (p *movePickerModel) addNewOptions(options []moveOption) {
	p.loadingNew = false
	p.options = append(p.options, options...)
	if p.cursor >= len(p.options) {
		p.cursor = max(0, len(p.options)-1)
	}
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
	if !p.active {
		return ""
	}

	var b strings.Builder
	b.WriteString(movePickerTitleStyle.Render(fmt.Sprintf("Move job #%d to:", p.jobID)))
	b.WriteString("\n")
	if strings.TrimSpace(p.status) != "" {
		b.WriteString("\n")
		b.WriteString(movePickerDimStyle.Render(p.status))
		b.WriteString("\n")
	}

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
	} else if p.existingDone {
		b.WriteString("\n")
		b.WriteString(movePickerSectionStyle.Render("EXISTING INSTANCES"))
		b.WriteString("\n")
		b.WriteString(movePickerDimStyle.Render("  none available"))
		b.WriteString("\n")
	}

	if hasNew || p.loadingNew {
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
		if p.loadingNew {
			b.WriteString(movePickerDimStyle.Render("  searching cloud offers..."))
			b.WriteString("\n")
		}
	}

	if len(p.options) == 0 && !p.loadingNew {
		b.WriteString("\n")
		b.WriteString(movePickerDimStyle.Render("No compatible destinations found."))
		b.WriteString("\n")
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
	return fmt.Sprintf("  %-12s %-14s ~%s wait  $%.2f/hr",
		ids.FormatInstanceID(o.instanceID), o.gpuName, dashboard.FormatCompactDuration(o.waitTime), o.costPerHour)
}

func movePickerStatus(existingCount, newCount int, loading bool) string {
	switch {
	case loading && existingCount > 0:
		return fmt.Sprintf("%d existing destination(s) ready; searching cloud offers...", existingCount)
	case loading:
		return "No existing destinations; searching cloud offers..."
	case existingCount > 0 || newCount > 0:
		return fmt.Sprintf("Found %d existing and %d new destination(s).", existingCount, newCount)
	default:
		return "No compatible destinations found."
	}
}

func countMoveOptions(options []moveOption) (existingCount, newCount int) {
	for _, opt := range options {
		if opt.isNew {
			newCount++
		} else {
			existingCount++
		}
	}
	return existingCount, newCount
}

func moveOptionsFromOrchestration(options []orchestration.Option) []moveOption {
	moveOptions := make([]moveOption, 0, len(options))
	for _, o := range options {
		moveOptions = append(moveOptions, moveOption{
			isNew:       o.IsNew,
			instanceID:  o.InstanceID,
			offer:       o.Offer,
			strategy:    o.Strategy,
			gpuName:     o.GPUName,
			waitTime:    o.WaitTime,
			costPerHour: o.CostPerHour,
		})
	}
	return moveOptions
}
