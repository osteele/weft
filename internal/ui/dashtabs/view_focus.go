package dashtabs

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/jobeta"
)

type focusView struct{}

func newFocusView() *focusView                        { return &focusView{} }
func (v *focusView) Title() string                    { return "Focus" }
func (v *focusView) ShortKey() string                 { return "4" }
func (v *focusView) Init() tea.Cmd                    { return nil }
func (v *focusView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

func (v *focusView) Render(width, height int, snap Snapshot, _ bool) string {
	var running []*db.Job
	for _, j := range snap.Jobs {
		if j == nil {
			continue
		}
		switch j.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			running = append(running, j)
		}
	}
	if len(running) == 0 {
		return dimStyle.Render("Focus — no jobs running right now.")
	}

	now := time.Now()
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("Focus — %d running", len(running))))
	b.WriteString("\n\n")
	for _, j := range running {
		b.WriteString(focusCard(j, width, snap.LaunchLiveByID, now))
		b.WriteString("\n")
	}
	_ = height
	return b.String()
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
		elapsed = humanDuration(d)
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

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
