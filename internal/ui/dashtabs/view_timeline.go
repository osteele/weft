package dashtabs

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/jobeta"
)

type timelineView struct{}

func newTimelineView() *timelineView                     { return &timelineView{} }
func (v *timelineView) Title() string                    { return "Timeline" }
func (v *timelineView) ShortKey() string                 { return "2" }
func (v *timelineView) Init() tea.Cmd                    { return nil }
func (v *timelineView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

func (v *timelineView) Render(width, height int, snap Snapshot, _ bool) string {
	now := time.Now()
	// 6-hour window: 3h past, 3h future.
	winStart := now.Add(-3 * time.Hour)
	winEnd := now.Add(3 * time.Hour)
	winDur := winEnd.Sub(winStart)

	jobs := runningOrQueued(snap.Jobs)
	if len(jobs) == 0 {
		return dimStyle.Render("Timeline — no running or queued jobs.")
	}

	const labelW = 28
	barW := width - labelW - 2
	if barW < 10 {
		return dimStyle.Render("(window too narrow for timeline)")
	}
	nowCol := int(float64(now.Sub(winStart)) / float64(winDur) * float64(barW))

	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("Timeline — last 3h ←|→ next 3h (%s now)", now.Format("15:04"))))
	b.WriteString("\n")

	// Sort: running first, then queued; within each, by start_time / queued_at.
	sort.Slice(jobs, func(i, j int) bool {
		a, b := jobs[i], jobs[j]
		ra, rb := isRunning(a), isRunning(b)
		if ra != rb {
			return ra
		}
		return effectiveStart(a) < effectiveStart(b)
	})

	rows := height - 3 // title + now-line + bottom axis
	if rows < 1 {
		rows = 1
	}
	if len(jobs) > rows {
		jobs = jobs[:rows]
	}

	for _, j := range jobs {
		label := jobShortLabel(j, labelW)
		bar := renderBar(j, winStart, winEnd, barW, nowCol, snap.LaunchLiveByID, now)
		b.WriteString(label)
		b.WriteString("  ")
		b.WriteString(bar)
		b.WriteString("\n")
	}

	// Axis.
	axis := make([]rune, barW)
	for i := range axis {
		axis[i] = '─'
	}
	if nowCol >= 0 && nowCol < barW {
		axis[nowCol] = '┼'
	}
	b.WriteString(strings.Repeat(" ", labelW+2))
	b.WriteString(dimStyle.Render(string(axis)))
	b.WriteString("\n")
	b.WriteString(strings.Repeat(" ", labelW+2))
	b.WriteString(dimStyle.Render(fmt.Sprintf("%s%s%s",
		winStart.Format("15:04"),
		strings.Repeat(" ", maxInt(0, barW-10)),
		winEnd.Format("15:04"))))
	return b.String()
}

func runningOrQueued(jobs []*db.Job) []*db.Job {
	out := []*db.Job{}
	for _, j := range jobs {
		if j == nil {
			continue
		}
		switch j.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting, db.StatusQueued, db.StatusPendingPlacement:
			out = append(out, j)
		}
	}
	return out
}

func isRunning(j *db.Job) bool {
	switch j.EffectiveStatus() {
	case db.StatusRunning, db.StatusStarting:
		return true
	}
	return false
}

func effectiveStart(j *db.Job) int64 {
	if j.StartTime > 0 {
		return j.StartTime
	}
	if j.QueuedAt > 0 {
		return j.QueuedAt
	}
	return j.CreatedAt
}

func jobShortLabel(j *db.Job, w int) string {
	id := fmt.Sprintf("wj%d", j.ID)
	desc := j.Description
	if desc == "" {
		desc = j.GeneratedDescription
	}
	if desc == "" {
		desc = j.Command
	}
	desc = singleLine(desc)
	full := fmt.Sprintf("%s %s", id, desc)
	if len(full) > w {
		full = full[:w-1] + "…"
	}
	return padRight(full, w)
}

func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func renderBar(j *db.Job, winStart, winEnd time.Time, barW, nowCol int, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) string {
	dur := winEnd.Sub(winStart)
	cells := make([]rune, barW)
	for i := range cells {
		cells[i] = ' '
	}
	// Filled part: from start (or queued_at) to now (running) or just a marker (queued).
	startUnix := effectiveStart(j)
	if startUnix <= 0 {
		// Queued without timestamp — show a small marker at "now".
		if nowCol >= 0 && nowCol < barW {
			cells[nowCol] = '◆'
		}
		return queuedStyle.Render(string(cells))
	}
	start := time.Unix(startUnix, 0)
	if start.Before(winStart) {
		start = winStart
	}
	startCol := int(float64(start.Sub(winStart)) / float64(dur) * float64(barW))

	var endCol int
	if isRunning(j) {
		endCol = nowCol
	} else {
		endCol = startCol // queued — point marker
	}
	if endCol < startCol {
		endCol = startCol
	}
	for i := startCol; i <= endCol && i < barW; i++ {
		if i < 0 {
			continue
		}
		cells[i] = '█'
	}
	// Projected ETA using the same estimator as `weft tui`.
	if isRunning(j) {
		if remaining, ok := jobeta.EstimateRunningJobRemaining(j, launchLiveByID, now); ok && remaining.Mean > 0 {
			etaTime := now.Add(remaining.Mean)
			if etaTime.After(winEnd) {
				etaTime = winEnd
			}
			etaCol := int(float64(etaTime.Sub(winStart)) / float64(dur) * float64(barW))
			for i := nowCol + 1; i <= etaCol && i < barW; i++ {
				if i < 0 {
					continue
				}
				cells[i] = '░'
			}
		}
	}
	style := runningStyle
	if !isRunning(j) {
		style = queuedStyle
	}
	return style.Render(string(cells))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (v *timelineView) Attention(_ Snapshot) Attention { return AttentionNone }
