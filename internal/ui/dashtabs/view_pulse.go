package dashtabs

import (
	"fmt"
	"math"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type pulseView struct{}

func newPulseView() *pulseView { return &pulseView{} }

func (v *pulseView) Title() string                    { return "Pulse" }
func (v *pulseView) ShortKey() string                 { return "1" }
func (v *pulseView) Init() tea.Cmd                    { return nil }
func (v *pulseView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

func (v *pulseView) Render(width, height int, snap Snapshot, focused bool) string {
	if width < 20 || height < 6 {
		return dimStyle.Render("(window too small)")
	}
	lines := []string{
		titleStyle.Render("Pulse — at-a-glance system state"),
		"",
		statusDonut(snap.Counts, width),
		"",
		titleStyle.Render("Sparklines (most-recent → older)"),
	}
	lines = append(lines, sparklineRow("queue depth", intsToFloats(snap.History.QueueDepth), width-14, "%.0f"))
	lines = append(lines, sparklineRow("running   ", intsToFloats(snap.History.RunningCnt), width-14, "%.0f"))
	lines = append(lines, sparklineRow("$/hr      ", snap.History.SpendPerHr, width-14, "$%.2f"))
	lines = append(lines, sparklineRow("failures  ", intsToFloats(snap.History.FailureCnt), width-14, "%.0f"))

	if len(snap.RecentFailures) > 0 {
		lines = append(lines, "")
		lines = append(lines, titleStyle.Render("Recent failure providers (24h)"))
		for _, f := range snap.RecentFailures {
			lines = append(lines, fmt.Sprintf("  %s · %d", failedStyle.Render(f.Provider), f.Count))
		}
	}

	body := strings.Join(lines, "\n")
	return lipgloss.NewStyle().MaxWidth(width).MaxHeight(height).Render(body)
}

func intsToFloats(xs []int) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = float64(x)
	}
	return out
}

// statusDonut renders a horizontal bar showing the relative share of each
// status. It's not literally a donut but conveys the same information in a
// terminal-friendly form.
func statusDonut(c StatusCounts, width int) string {
	total := c.Running + c.Queued + c.Completed + c.Failed + c.Killed + c.Other
	if total == 0 {
		return dimStyle.Render("no jobs")
	}
	bar := func(label string, n int, style lipgloss.Style) string {
		if n == 0 {
			return ""
		}
		w := int(math.Round(float64(n) / float64(total) * float64(width-1)))
		if w < 1 {
			w = 1
		}
		return style.Render(strings.Repeat("█", w)) + " " + style.Render(fmt.Sprintf("%s %d", label, n)) + "  "
	}
	parts := []string{}
	if s := bar("running", c.Running, runningStyle); s != "" {
		parts = append(parts, s)
	}
	if s := bar("queued", c.Queued, queuedStyle); s != "" {
		parts = append(parts, s)
	}
	if s := bar("done", c.Completed, completedStyle); s != "" {
		parts = append(parts, s)
	}
	if s := bar("failed", c.Failed, failedStyle); s != "" {
		parts = append(parts, s)
	}
	if s := bar("killed", c.Killed, dimStyle); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "")
}

// sparklineRow renders a labeled Unicode block sparkline.
//
// We use the eight block characters (one through eight eighths) rather than
// braille — they're simpler and render reliably across terminal fonts.
// History is most-recent-first; we reverse it for left-to-right reading.
func sparklineRow(label string, values []float64, width int, valueFmt string) string {
	if width < 8 {
		width = 8
	}
	if len(values) == 0 {
		return fmt.Sprintf("  %s  %s", dimStyle.Render(label), dimStyle.Render("(no data)"))
	}
	// Trim to fit and reverse for left→right time order.
	if len(values) > width {
		values = values[:width]
	}
	rev := make([]float64, len(values))
	for i := range values {
		rev[i] = values[len(values)-1-i]
	}
	max := rev[0]
	for _, v := range rev {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		max = 1
	}
	const blocks = " ▁▂▃▄▅▆▇█"
	bs := []rune(blocks)
	sb := strings.Builder{}
	for _, v := range rev {
		idx := int(math.Round(v / max * float64(len(bs)-1)))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(bs) {
			idx = len(bs) - 1
		}
		sb.WriteRune(bs[idx])
	}
	latest := rev[len(rev)-1]
	return fmt.Sprintf("  %s  %s  %s",
		dimStyle.Render(label),
		accentStyle.Render(sb.String()),
		accentStyle.Render(fmt.Sprintf(valueFmt, latest)),
	)
}
