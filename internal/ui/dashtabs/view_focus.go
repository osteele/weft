package dashtabs

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/jobeta"
)

type focusView struct{}

func newFocusView() *focusView                        { return &focusView{} }
func (v *focusView) Title() string                    { return "Focus" }
func (v *focusView) ShortKey() string                 { return "4" }
func (v *focusView) Init() tea.Cmd                    { return nil }
func (v *focusView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

func (v *focusView) Render(width, height int, snap Snapshot, _ bool) string {
	now := time.Now()
	var running []*db.Job
	var recent []*db.Job
	recentCutoff := now.Add(-6 * time.Hour).Unix()
	for _, j := range snap.Jobs {
		if j == nil {
			continue
		}
		switch j.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			running = append(running, j)
		case db.StatusCompleted, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			if j.EndTime != nil && *j.EndTime >= recentCutoff {
				recent = append(recent, j)
			}
		}
	}
	// Newest-first for recent.
	sort.Slice(recent, func(i, j int) bool {
		a, b := int64(0), int64(0)
		if recent[i].EndTime != nil {
			a = *recent[i].EndTime
		}
		if recent[j].EndTime != nil {
			b = *recent[j].EndTime
		}
		return a > b
	})

	var b strings.Builder
	if len(running) == 0 {
		b.WriteString(dimStyle.Render("Focus — no jobs running right now."))
	} else {
		b.WriteString(titleStyle.Render(fmt.Sprintf("Focus — %d running", len(running))))
		b.WriteString("\n\n")
		for _, j := range running {
			b.WriteString(focusCard(j, width, snap.LaunchLiveByID, now))
			b.WriteString("\n")
		}
	}

	// Recently-completed section, shown when there's room. Each card is
	// ~6 rows tall; budget the rest of the body height to one-line recents.
	usedRows := 2 + 6*len(running)
	if remaining := height - usedRows; remaining >= 3 && len(recent) > 0 {
		b.WriteString("\n")
		b.WriteString(titleStyle.Render(fmt.Sprintf("RECENT — %d completed in last 6h", len(recent))))
		b.WriteString("\n")
		shown := remaining - 2
		if shown > len(recent) {
			shown = len(recent)
		}
		for i := 0; i < shown; i++ {
			b.WriteString(recentJobLine(recent[i], width))
			b.WriteString("\n")
		}
		if more := len(recent) - shown; more > 0 {
			b.WriteString(dimStyle.Render(fmt.Sprintf("  … %d more", more)))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// recentJobLine renders one row in the Focus tab's "RECENT" section: outcome
// glyph, id, project, description, duration, cost, when.
func recentJobLine(j *db.Job, width int) string {
	if j == nil {
		return ""
	}
	id := fmt.Sprintf("wj%d", j.ID)
	desc := singleLine(j.Description)
	if desc == "" {
		desc = singleLine(j.GeneratedDescription)
	}
	if desc == "" {
		desc = singleLine(j.Command)
	}
	var glyph string
	var glyphStyle = completedStyle
	switch j.EffectiveStatus() {
	case db.StatusCompleted:
		if j.ExitCode != nil && *j.ExitCode != 0 {
			glyph = "✗"
			glyphStyle = failedStyle
		} else {
			glyph = "✓"
			glyphStyle = runningStyle
		}
	case db.StatusFailed:
		glyph = "✗"
		glyphStyle = failedStyle
	case db.StatusKilled, db.StatusCanceled:
		glyph = "▢"
		glyphStyle = dimStyle
	default:
		glyph = "·"
	}
	var when, dur string
	if j.EndTime != nil {
		when = estimate.FormatDurationShort(time.Since(time.Unix(*j.EndTime, 0))) + " ago"
		if j.StartTime > 0 {
			dur = estimate.FormatDurationShort(time.Unix(*j.EndTime, 0).Sub(time.Unix(j.StartTime, 0)))
		}
	}
	cost := ""
	if j.Cost != nil && *j.Cost > 0 {
		cost = fmt.Sprintf("$%.2f", *j.Cost)
	}

	// Compose a fixed-prefix line, then a description column padded so the
	// suffix is right-justified against a fixed column width. That keeps
	// "5h22m ago" (short suffix) and "in 28m  45m ago" (long suffix) sharing
	// the same right edge across rows.
	prefix := fmt.Sprintf("  %s %s  %-22s ",
		glyphStyle.Render(glyph),
		accentStyle.Render(id),
		shortStr(j.Project, 22),
	)
	suffixParts := []string{}
	if dur != "" {
		suffixParts = append(suffixParts, "in "+dur)
	}
	if when != "" {
		suffixParts = append(suffixParts, when)
	}
	if cost != "" {
		suffixParts = append(suffixParts, cost)
	}
	const suffixColW = 24 // accommodates "in NhNNm  NhNNm ago  $NN.NN"
	suffixRaw := strings.Join(suffixParts, "  ")
	if pad := suffixColW - len(suffixRaw); pad > 0 {
		suffixRaw = strings.Repeat(" ", pad) + suffixRaw
	}
	suffix := dimStyle.Render(suffixRaw)
	prefixW := lipgloss.Width(prefix)
	available := width - prefixW - suffixColW - 2
	if available < 10 {
		available = 10
	}
	descCell := dimStyle.Render(padRight(shortStr(desc, available), available))
	return prefix + descCell + "  " + suffix
}

func focusCard(j *db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) string {
	id := fmt.Sprintf("wj%d", j.ID)
	desc := j.Description
	if desc == "" {
		desc = j.GeneratedDescription
	}
	if desc == "" {
		desc = j.Command
	}
	desc = singleLine(desc)

	elapsed := ""
	if j.StartTime > 0 {
		d := now.Sub(time.Unix(j.StartTime, 0))
		elapsed = estimate.FormatDurationShort(d)
	}
	host := j.Host
	if host == "" && j.LaunchID != nil {
		host = fmt.Sprintf("rental#%d", *j.LaunchID)
	}
	gpu := j.GPUClass
	if gpu == "" {
		gpu = j.GPU
	}
	cost := ""
	if j.Cost != nil {
		cost = fmt.Sprintf("$%.2f", *j.Cost)
	}

	frac := jobeta.JobProgressFraction(j, launchLiveByID, now)
	remaining, hasETA := jobeta.EstimateRunningJobRemaining(j, launchLiveByID, now)
	progress := renderProgressBar(frac, width-4)
	etaText := "—"
	if hasETA && remaining.Mean > 0 {
		etaText = remaining.FormatWithBounds()
	}

	lines := []string{
		fmt.Sprintf("%s  %s", titleStyle.Render(id), runningStyle.Render(j.Project)),
		dimStyle.Render(shortStr(desc, width-2)),
		progress,
		fmt.Sprintf("  %s  %s  %s  %s  %s",
			dimStyle.Render("host: "+host),
			dimStyle.Render("gpu: "+gpu),
			dimStyle.Render("elapsed: "+elapsed),
			dimStyle.Render("ETA "+etaText),
			dimStyle.Render("cost: "+cost)),
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorDim).
		Padding(0, 1).
		Width(width - 2).
		Render(strings.Join(lines, "\n"))
}

func renderProgressBar(frac float64, width int) string {
	if width < 10 {
		width = 10
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(float64(width) * frac)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	bar := runningStyle.Render(strings.Repeat("█", filled)) + dimStyle.Render(strings.Repeat("░", width-filled))
	return "  " + bar + dimStyle.Render(fmt.Sprintf("  ≈%.0f%%", frac*100))
}
