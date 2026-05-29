package dashtabs

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

type historyView struct{}

func newHistoryView() *historyView                      { return &historyView{} }
func (v *historyView) Title() string                    { return "History" }
func (v *historyView) ShortKey() string                 { return "7" }
func (v *historyView) Init() tea.Cmd                    { return nil }
func (v *historyView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

func (v *historyView) Render(width, height int, snap Snapshot, _ bool) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("History — last 24h instance outcomes"))
	b.WriteString("\n\n")

	// Stacked bar of recent instances.
	bar, summary := stackedInstanceBar(snap.CloudInstances(), width-4)
	b.WriteString("  ")
	b.WriteString(bar)
	b.WriteString("\n  ")
	b.WriteString(dimStyle.Render(summary))
	b.WriteString("\n\n")

	// Per-provider 24-hour outcome heatmap.
	rows := providerHourlyHeatmap(snap.CloudInstances())
	if len(rows) == 0 {
		b.WriteString(dimStyle.Render("(no instance history in last 24h)"))
	} else {
		b.WriteString(titleStyle.Render("By provider × hour (now → 24h ago, left to right)"))
		b.WriteString("\n")
		for _, r := range rows {
			b.WriteString("  " + r + "\n")
		}
	}
	_ = height
	return b.String()
}

func stackedInstanceBar(launches []*db.Launch, width int) (string, string) {
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	var ok, failed, dud, ongoing, awaiting int
	for _, l := range launches {
		if l == nil {
			continue
		}
		if l.EndedAt != nil && *l.EndedAt < cutoff {
			continue
		}
		switch l.Status {
		case db.LaunchStatusCompleted:
			ok++
		case db.LaunchStatusFailed:
			if strings.Contains(strings.ToLower(l.TerminationReason), "dud") {
				dud++
			} else {
				failed++
			}
		case db.LaunchStatusRunning:
			ongoing++
		case db.LaunchStatusPlanned, db.LaunchStatusLaunching:
			awaiting++
		}
	}
	total := ok + failed + dud + ongoing + awaiting
	if total == 0 {
		return dimStyle.Render(strings.Repeat("░", width)), "no events"
	}
	type piece struct {
		count int
		ch    string
		style lipgloss.Style
	}
	pieces := []piece{
		{ok, "█", runningStyle},
		{ongoing, "▓", queuedStyle},
		{awaiting, "▒", dimStyle},
		{dud, "░", queuedStyle},
		{failed, "█", failedStyle},
	}
	var sb strings.Builder
	used := 0
	for i, p := range pieces {
		if p.count == 0 {
			continue
		}
		w := p.count * width / total
		if i == len(pieces)-1 && used+w < width {
			w = width - used
		}
		if w < 1 {
			w = 1
		}
		used += w
		sb.WriteString(p.style.Render(strings.Repeat(p.ch, w)))
	}
	summary := fmt.Sprintf("%d ok · %d ongoing · %d awaiting · %d dud · %d fail",
		ok, ongoing, awaiting, dud, failed)
	return sb.String(), summary
}

// providerHourlyHeatmap returns one line per provider, with 24 cells from
// most-recent (left) to 24h-ago (right). Each cell is shaded by failure rate.
func providerHourlyHeatmap(launches []*db.Launch) []string {
	now := time.Now()
	cutoff := now.Add(-24 * time.Hour).Unix()
	type bucket struct{ total, failed int }
	byProv := map[string][24]bucket{}
	for _, l := range launches {
		if l == nil {
			continue
		}
		ts := int64(0)
		if l.EndedAt != nil {
			ts = *l.EndedAt
		} else if l.LaunchedAt != nil {
			ts = *l.LaunchedAt
		}
		if ts == 0 || ts < cutoff {
			continue
		}
		ago := int(now.Sub(time.Unix(ts, 0)).Hours())
		if ago < 0 || ago >= 24 {
			continue
		}
		bs := byProv[l.Provider]
		b := bs[ago]
		b.total++
		if l.Status == db.LaunchStatusFailed {
			b.failed++
		}
		bs[ago] = b
		byProv[l.Provider] = bs
	}

	provs := make([]string, 0, len(byProv))
	for k := range byProv {
		provs = append(provs, k)
	}
	sort.Strings(provs)

	rows := []string{}
	for _, p := range provs {
		bs := byProv[p]
		var sb strings.Builder
		sb.WriteString(dimStyle.Render(fmt.Sprintf("%-10s ", p)))
		for h := 0; h < 24; h++ {
			b := bs[h]
			switch {
			case b.total == 0:
				sb.WriteString(dimStyle.Render("·"))
			case b.failed == 0:
				sb.WriteString(runningStyle.Render("█"))
			case b.failed == b.total:
				sb.WriteString(failedStyle.Render("█"))
			default:
				sb.WriteString(queuedStyle.Render("▓"))
			}
		}
		rows = append(rows, sb.String())
	}
	return rows
}
