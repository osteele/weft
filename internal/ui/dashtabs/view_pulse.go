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
		titleStyle.Render("Pulse — job counts, spend rate, recent failures"),
		"",
		statusDonut(snap.Counts, width),
		"",
		titleStyle.Render("Trends (sparkline = older → newer)"),
	}
	// Labels are pre-padded to a common width so the sparkline column and
	// the latest-value column line up across all rows.
	const labelW = 12
	lines = append(lines, sparklineRow(padRight("queue depth", labelW), intsToFloats(snap.History.QueueDepth), width-14, "%.0f"))
	lines = append(lines, sparklineRow(padRight("running", labelW), intsToFloats(snap.History.RunningCnt), width-14, "%.0f"))
	lines = append(lines, sparklineRow(padRight("$/hr", labelW), snap.History.SpendPerHr, width-14, "$%.2f"))
	lines = append(lines, sparklineRow(padRight("failures", labelW), intsToFloats(snap.History.FailureCnt), width-14, "%.0f"))

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

// sparklineMinSamples is the smallest sample count we'll render as a
// sparkline. Below this, the line lacks enough variation to read as a trend
// and a single full-height block reads as misleading. We show the current
// value prominently in that warm-up window and use a "warming up" hint.
const sparklineMinSamples = 5

// sparklineRow renders a labeled value with an optional Unicode block
// sparkline as a faint trail behind it (Option 2 layout). When there are
// fewer than sparklineMinSamples in the history, the sparkline is replaced
// by a dim "(warming up · N/5)" tag so the user knows it'll fill in.
//
// History is most-recent-first; we reverse it for left-to-right reading
// (oldest sample at the left, newest at the right, matching the column
// header "most-recent → older" we abandoned).
func sparklineRow(label string, values []float64, width int, valueFmt string) string {
	if width < 8 {
		width = 8
	}
	// Determine current value (most-recent sample), independent of whether
	// we have enough for a sparkline.
	current := ""
	if len(values) > 0 {
		current = fmt.Sprintf(valueFmt, values[0])
	} else {
		current = "—"
	}

	// Warm-up state: emphasize the value, show a dim hint about progress.
	if len(values) < sparklineMinSamples {
		hint := fmt.Sprintf("(warming up · %d/%d)", len(values), sparklineMinSamples)
		return fmt.Sprintf("  %s  %s  %s",
			dimStyle.Render(label),
			accentStyle.Bold(true).Render(current),
			dimStyle.Render(hint),
		)
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
	// Render the sparkline in the dim style so it reads as a trail behind
	// the bolded current value, per Option 2.
	return fmt.Sprintf("  %s  %s  %s",
		dimStyle.Render(label),
		dimStyle.Render(sb.String()),
		accentStyle.Bold(true).Render(current),
	)
}
