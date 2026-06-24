package dashboard

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/queueblock"
)

// JobItem wraps a db.Job for use in bubbles/list
type JobItem struct {
	Job *db.Job
}

// FilterValue implements list.Item interface - used for filtering
func (i JobItem) FilterValue() string {
	return i.Job.EffectiveDescription()
}

// JobDelegate handles rendering of job items in the list
type JobDelegate struct {
	styles jobListStyles
}

type jobListStyles struct {
	normal   lipgloss.Style
	selected lipgloss.Style
	running  lipgloss.Style
	failed   lipgloss.Style
	pending  lipgloss.Style
	paused   lipgloss.Style
	dead     lipgloss.Style
	draft    lipgloss.Style
}

func newJobListStyles() jobListStyles {
	return jobListStyles{
		normal:   lipgloss.NewStyle(),
		selected: lipgloss.NewStyle().Background(lipgloss.Color("240")).Bold(true),
		running:  lipgloss.NewStyle().Foreground(lipgloss.Color("10")), // green
		failed:   lipgloss.NewStyle().Foreground(lipgloss.Color("9")),  // red
		pending:  lipgloss.NewStyle().Foreground(lipgloss.Color("11")), // yellow
		paused:   lipgloss.NewStyle().Foreground(lipgloss.Color("13")), // magenta
		dead:     lipgloss.NewStyle().Foreground(lipgloss.Color("8")),  // gray
		draft:    lipgloss.NewStyle().Foreground(lipgloss.Color("14")), // cyan
	}
}

// NewJobDelegate creates a new delegate for rendering job items
func NewJobDelegate() JobDelegate {
	return JobDelegate{
		styles: newJobListStyles(),
	}
}

// Height returns how many lines each item takes
func (d JobDelegate) Height() int {
	return 1
}

// Spacing returns the gap between items
func (d JobDelegate) Spacing() int {
	return 0
}

// Update handles item-level events
func (d JobDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd {
	return nil
}

// Render renders a single job item
func (d JobDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	jobItem, ok := item.(JobItem)
	if !ok {
		return
	}
	job := jobItem.Job

	isSelected := index == m.Index()

	// Build status indicator
	statusStr := formatJobStatus(job)

	// Build time display
	timeStr := "—"
	if job.StartTime > 0 {
		timeStr = time.Unix(job.StartTime, 0).Format("01/02 15:04")
	}

	// Build description/command display
	display := job.EffectiveDescription()
	// Strip leading backslash-newline (shell line continuation)
	display = strings.TrimPrefix(display, "\\\n")
	// Replace all newlines with spaces for single-line display
	display = strings.ReplaceAll(display, "\n", " ")
	display = strings.TrimSpace(display)

	// Calculate available width for description
	// Format: ID(6) + space + Host(10) + space + Status(12) + space + Time(11) + space + Desc
	// Leave room for description
	idStr := fmt.Sprintf("%5d", job.ID)
	hostStr := truncateOrPad(job.Host, 10)

	// Build the line
	line := fmt.Sprintf("%s %s %s %s %s",
		idStr,
		hostStr,
		truncateOrPad(statusStr, 12),
		timeStr,
		display,
	)

	// Truncate to list width if needed (using visual width, not byte length)
	listWidth := m.Width()
	if listWidth > 0 && runewidth.StringWidth(line) > listWidth {
		line = truncateToWidth(line, listWidth-1) + "…"
	}

	// Apply styles
	style := d.styles.normal
	if isSelected {
		style = d.styles.selected
	}

	// Apply status-based coloring (for the whole line when not selected)
	if !isSelected {
		switch job.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			style = d.styles.running
		case db.StatusPaused:
			style = d.styles.paused
		case db.StatusCompleted:
			if job.ExitCode != nil && *job.ExitCode != 0 {
				style = d.styles.failed
			}
		case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			style = d.styles.dead
		case db.StatusQueued:
			style = d.styles.pending
		case db.StatusDraft:
			style = d.styles.draft
		}
	}

	// Render with width padding
	if listWidth > 0 {
		line = truncateOrPad(line, listWidth)
	}

	fmt.Fprint(w, style.Render(line))
}

// formatJobStatus returns a display string for the job status
func formatJobStatus(job *db.Job) string {
	// If there's a pending status, show it with an indicator
	if job.PendingStatus != nil {
		if job.EffectiveStatus() != *job.PendingStatus {
			return formatActualStatus(job)
		}
		return formatPendingStatus(*job.PendingStatus)
	}

	return formatActualStatus(job)
}

// formatActualStatus returns display string for the verified status
func formatActualStatus(job *db.Job) string {
	if display := queueblock.Display(job, nil); display.Kind != "" {
		switch display.Kind {
		case queueblock.KindBlocked:
			return "◌ blocked"
		case queueblock.KindPaused:
			return "◌ paused"
		case queueblock.KindWaiting:
			return "◌ waiting"
		}
	}
	switch job.EffectiveStatus() {
	case db.StatusCompleted:
		if job.ExitCode != nil {
			if *job.ExitCode == 0 {
				return "✓ done"
			}
			return fmt.Sprintf("✗ exit %d", *job.ExitCode)
		}
		return "completed"
	case db.StatusRunning:
		return "● running"
	case db.StatusStarting:
		return "○ starting"
	case db.StatusQueued:
		return "◌ queued"
	case db.StatusPaused:
		return "⏸ paused"
	case db.StatusDead:
		return "✗ start failed"
	case db.StatusFailed:
		return "✗ crashed"
	case db.StatusKilled:
		return "✗ killed"
	case db.StatusCanceled:
		return "✗ canceled"
	case db.StatusDraft:
		return "✎ draft"
	default:
		return job.EffectiveStatus()
	}
}

// formatPendingStatus returns display string for pending (target) status
func formatPendingStatus(status string) string {
	switch status {
	case db.StatusDead:
		return "⧗ killing…"
	case db.StatusKilled:
		return "⧗ killing…"
	case db.StatusCanceled:
		return "⧗ canceling…"
	case db.StatusRunning:
		return "⧗ starting…"
	case db.StatusQueued:
		return "⧗ queuing…"
	case db.StatusPaused:
		return "⧗ pausing…"
	case db.StatusDraft:
		return "⧗ drafting…"
	default:
		return "⧗ " + status + "…"
	}
}

// truncateToWidth truncates a string to the given visual width (not byte length)
func truncateToWidth(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}

	currentWidth := 0
	runes := []rune(s)

	for i, r := range runes {
		charWidth := runewidth.RuneWidth(r)
		if currentWidth+charWidth > maxWidth {
			return string(runes[:i])
		}
		currentWidth += charWidth
	}

	return s
}

// truncateOrPad truncates or pads a string to the given visual width (not byte length)
func truncateOrPad(s string, width int) string {
	currentWidth := runewidth.StringWidth(s)
	if currentWidth > width {
		return truncateToWidth(s, width-1) + "…"
	}
	return s + strings.Repeat(" ", width-currentWidth)
}

// JobsToListItems converts a slice of jobs to list items
func JobsToListItems(jobs []*db.Job) []list.Item {
	items := make([]list.Item, len(jobs))
	for i, job := range jobs {
		items[i] = JobItem{Job: job}
	}
	return items
}
