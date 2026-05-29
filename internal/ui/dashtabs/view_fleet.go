package dashtabs

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
)

type fleetView struct{}

func newFleetView() *fleetView                        { return &fleetView{} }
func (v *fleetView) Title() string                    { return "Fleet" }
func (v *fleetView) ShortKey() string                 { return "3" }
func (v *fleetView) Init() tea.Cmd                    { return nil }
func (v *fleetView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

// cellWidth is the target width for each host/instance card. We pack as many
// columns as fit in the available width.
const fleetCellWidth = 22
const fleetCellHeight = 6

func (v *fleetView) Render(width, height int, snap Snapshot, _ bool) string {
	cards := []string{}
	for _, h := range snap.Hosts {
		cards = append(cards, hostCard(h, snap.Jobs))
	}
	for _, l := range snap.LiveInstances {
		if l == nil {
			continue
		}
		cards = append(cards, instanceCard(l, snap.Jobs))
	}

	cols := width / (fleetCellWidth + 1)
	if cols < 1 {
		cols = 1
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("Fleet — %d hosts, %d live cloud instances", len(snap.Hosts), len(snap.LiveInstances))))
	if n := len(snap.StaleInstances); n > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  (+%d stale non-running — see Alerts)", n)))
	}
	b.WriteString("\n\n")
	if len(cards) == 0 {
		b.WriteString(dimStyle.Render("(no hosts or live instances visible)"))
		return b.String()
	}
	for row := 0; row < len(cards); row += cols {
		end := row + cols
		if end > len(cards) {
			end = len(cards)
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, cards[row:end]...))
		b.WriteString("\n")
	}
	_ = height
	return b.String()
}

func hostCard(h *hostinfo.Host, jobs []*db.Job) string {
	if h == nil {
		return ""
	}
	running := []*db.Job{}
	queued := 0
	for _, j := range jobs {
		if j == nil || j.Host != h.Name {
			continue
		}
		switch j.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			running = append(running, j)
		case db.StatusQueued, db.StatusPendingPlacement:
			queued++
		}
	}
	sort.Slice(running, func(i, j int) bool { return running[i].StartTime < running[j].StartTime })

	header := titleStyle.Render(h.Name)
	gpuLine := dimStyle.Render(hostGPUSummary(h))
	jobLine := ""
	if len(running) > 0 {
		jobLine = runningStyle.Render(fmt.Sprintf("▶ %d running", len(running)))
	} else {
		jobLine = dimStyle.Render("idle")
	}
	queueLine := ""
	if queued > 0 {
		queueLine = queuedStyle.Render(fmt.Sprintf("%d queued", queued))
	}
	statusLine := dimStyle.Render(hostStatusLabel(h.Status))

	lines := []string{header, gpuLine, jobLine}
	if queueLine != "" {
		lines = append(lines, queueLine)
	}
	lines = append(lines, statusLine)
	return cardBox(lines)
}

func hostGPUSummary(h *hostinfo.Host) string {
	if h == nil || len(h.GPUs) == 0 {
		return "no GPU"
	}
	name := h.GPUs[0].Name
	if name == "" {
		name = "GPU"
	}
	return fmt.Sprintf("%dx %s", len(h.GPUs), shortStr(name, 14))
}

func instanceCard(l *db.Launch, jobs []*db.Job) string {
	header := titleStyle.Render(fmt.Sprintf("wi%d", l.ID))
	gpuLine := dimStyle.Render(fmt.Sprintf("%s %s", l.Provider, shortStr(l.GPUSpec, 14)))

	displayStatus := instanceDisplayStatus(l)
	var statusLine string
	switch displayStatus {
	case "running":
		statusLine = runningStyle.Render(displayStatus)
	case "launching":
		statusLine = queuedStyle.Render(displayStatus)
	case "failed", "terminated":
		statusLine = failedStyle.Render(displayStatus)
	default:
		statusLine = completedStyle.Render(displayStatus)
	}
	rate := dimStyle.Render(fmt.Sprintf("$%.2f/hr", float64(l.CostPerHourCents)/100.0))
	uptime := dimStyle.Render(launchUptime(l))

	active, queued := countLaunchJobs(l.ID, jobs)
	jobLabel := ""
	switch {
	case active > 0 && queued > 0:
		jobLabel = runningStyle.Render(fmt.Sprintf("▶ %d", active)) + " " + queuedStyle.Render(fmt.Sprintf("+%d queued", queued))
	case active > 0:
		jobLabel = runningStyle.Render(fmt.Sprintf("▶ %d active", active))
	case queued > 0:
		jobLabel = queuedStyle.Render(fmt.Sprintf("%d queued", queued))
	}

	lines := []string{header, gpuLine, statusLine, rate, uptime}
	if jobLabel != "" {
		lines = append(lines, jobLabel)
	}
	return cardBox(lines)
}

// instanceDisplayStatus refines the raw launch status with information from
// related fields so the card reflects what's actually happening — not just
// what the provider reports. In particular, a launch whose status is "running"
// but whose agent has not yet reached ready is still bootstrapping; we show
// it as "launching" so it doesn't claim to be doing work it cannot do.
func instanceDisplayStatus(l *db.Launch) string {
	if l == nil {
		return "?"
	}
	if l.Status == db.LaunchStatusRunning && l.AgentReadyAtUnix == nil {
		return "launching"
	}
	return l.Status
}

func countLaunchJobs(launchID int64, jobs []*db.Job) (active, queued int) {
	for _, j := range jobs {
		if j == nil || j.LaunchID == nil || *j.LaunchID != launchID {
			continue
		}
		switch j.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			active++
		case db.StatusQueued, db.StatusPendingPlacement:
			queued++
		}
	}
	return active, queued
}

func launchUptime(l *db.Launch) string {
	if l == nil || l.LaunchedAt == nil {
		return "—"
	}
	d := time.Since(time.Unix(*l.LaunchedAt, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func cardBox(lines []string) string {
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorDim).
		Width(fleetCellWidth).
		Height(fleetCellHeight).
		Padding(0, 1)
	return style.Render(strings.Join(lines, "\n"))
}

func hostStatusLabel(s hostinfo.HostStatus) string {
	switch s {
	case hostinfo.HostStatusOnline:
		return "online"
	case hostinfo.HostStatusOffline:
		return "offline"
	case hostinfo.HostStatusChecking:
		return "checking"
	default:
		return "unknown"
	}
}

func shortStr(s string, w int) string {
	if len(s) <= w {
		return s
	}
	return s[:w-1] + "…"
}
