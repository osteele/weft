package dashtabs

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/llmusage"
)

type usageView struct{}

func newUsageView() *usageView                        { return &usageView{} }
func (v *usageView) Title() string                    { return "Usage" }
func (v *usageView) ShortKey() string                 { return "0" } // "0" = the tenth key on a numeric row
func (v *usageView) Init() tea.Cmd                    { return nil }
func (v *usageView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

// Render shows quantitative usage signals: LLM API calls and tokens (Anthropic
// is wired; OpenRouter will appear once that path records usage), and stubs
// for compute-hours / data-transfer when those signals get recorded.
func (v *usageView) Render(width, height int, snap Snapshot, _ bool) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Usage — quantities (calls, tokens)"))
	b.WriteString("\n\n")

	// LLM section.
	b.WriteString(titleStyle.Render("LLM API · last 24h"))
	b.WriteString("\n")
	if len(snap.LLMUsage24h) == 0 {
		b.WriteString(dimStyle.Render("  (no recorded calls in last 24h — Anthropic wiring is live; OpenRouter is not yet)\n"))
	} else {
		b.WriteString(renderLLMTable(snap.LLMUsage24h, width))
	}

	b.WriteString("\n")
	b.WriteString(titleStyle.Render("LLM API · last 7 days"))
	b.WriteString("\n")
	if len(snap.LLMUsage7d) == 0 {
		b.WriteString(dimStyle.Render("  (no recorded calls in last 7d)\n"))
	} else {
		b.WriteString(renderLLMTable(snap.LLMUsage7d, width))
	}

	// Daily spend sparkline.
	b.WriteString("\n")
	b.WriteString(titleStyle.Render("Daily LLM spend (last 30 days, oldest → newest)"))
	b.WriteString("\n")
	b.WriteString(renderDailySpendSparkline(snap.LLMDailySpend, width-4))
	b.WriteString("\n")

	// Compute: GPU-hours accrued per window, split cloud vs on-prem.
	b.WriteString("\n")
	b.WriteString(titleStyle.Render("Compute · GPU-hours"))
	b.WriteString("\n")
	b.WriteString(renderGPUHours(snap))

	// Data transfer: HF downloads + R2 uploads (cloud jobs only — the byte
	// counters are written by the cloud wrapper, not on-prem syncs).
	b.WriteString("\n")
	b.WriteString(titleStyle.Render("Data transfer · HF + R2") + dimStyle.Render("   (cloud jobs only)"))
	b.WriteString("\n")
	b.WriteString(renderTransfer(snap))

	_ = height
	return b.String()
}

// renderGPUHours shows GPU-hours over the 24h/7d/30d windows with a cloud +
// on-prem split.
func renderGPUHours(snap Snapshot) string {
	windows := []struct {
		label string
		u     UsageRollup
	}{
		{"last 24h", snap.Usage24h},
		{"last 7d", snap.Usage7d},
		{"last 30d", snap.Usage30d},
	}
	if snap.Usage24h.GPUHoursTotal() == 0 &&
		snap.Usage7d.GPUHoursTotal() == 0 &&
		snap.Usage30d.GPUHoursTotal() == 0 {
		return dimStyle.Render("  (none recorded)\n")
	}
	var b strings.Builder
	for _, w := range windows {
		b.WriteString(fmt.Sprintf("  %-9s %s   %s\n",
			dimStyle.Render(w.label),
			accentStyle.Render(fmt.Sprintf("%8.1f GPU-h", w.u.GPUHoursTotal())),
			dimStyle.Render(fmt.Sprintf("(cloud %.1f · on-prem %.1f)",
				w.u.GPUHoursCloud, w.u.GPUHoursOnprem))))
	}
	return b.String()
}

// renderTransfer shows HF download + R2 upload byte totals per window.
func renderTransfer(snap Snapshot) string {
	windows := []struct {
		label string
		u     UsageRollup
	}{
		{"last 24h", snap.Usage24h},
		{"last 7d", snap.Usage7d},
		{"last 30d", snap.Usage30d},
	}
	if snap.Usage24h.HFDownBytes == 0 && snap.Usage24h.R2UpBytes == 0 &&
		snap.Usage7d.HFDownBytes == 0 && snap.Usage7d.R2UpBytes == 0 &&
		snap.Usage30d.HFDownBytes == 0 && snap.Usage30d.R2UpBytes == 0 {
		return dimStyle.Render("  (none recorded)\n")
	}
	var b strings.Builder
	for _, w := range windows {
		b.WriteString(fmt.Sprintf("  %-9s %s down · %s up\n",
			dimStyle.Render(w.label),
			accentStyle.Render(fmt.Sprintf("HF %8s", formatBytes(w.u.HFDownBytes))),
			accentStyle.Render(fmt.Sprintf("R2 %8s", formatBytes(w.u.R2UpBytes)))))
	}
	return b.String()
}

func renderLLMTable(rows []llmusage.FeatureUsage, width int) string {
	if len(rows) == 0 {
		return dimStyle.Render("  (empty)")
	}
	// Columns: feature | model | calls | in tok | out tok | cache rd | cost
	header := fmt.Sprintf("  %-9s  %-22s  %6s  %10s  %10s  %10s  %8s",
		"feature", "model", "calls", "in tok", "out tok", "cache rd", "USD")
	var b strings.Builder
	b.WriteString(dimStyle.Render(header))
	b.WriteString("\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %-9s  %-22s  %6d  %10s  %10s  %10s  %s\n",
			r.Feature,
			shortStr(r.Model, 22),
			r.Calls,
			formatTokens(r.InputTokens),
			formatTokens(r.OutputTokens),
			formatTokens(r.CacheReadTokens),
			accentStyle.Render(fmt.Sprintf("$%6.4f", r.USD())),
		))
	}
	_ = width
	return b.String()
}

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// formatBytes renders a byte count in IEC units (KiB/MiB/GiB/TiB). Local to the
// dashtabs package because the byte-formatters elsewhere are unexported.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func renderDailySpendSparkline(days []llmusage.DaySpend, width int) string {
	if len(days) == 0 {
		return dimStyle.Render("  (no daily spend data)")
	}
	// days is newest first; reverse for left=oldest, right=newest reading.
	rev := make([]float64, len(days))
	for i, d := range days {
		rev[len(days)-1-i] = float64(d.CostMicros) / 1_000_000.0
	}
	total := 0.0
	for _, d := range days {
		total += float64(d.CostMicros) / 1_000_000.0
	}
	bars := sparkBars(rev, width)
	rangeLabel := fmt.Sprintf("(%s … %s)",
		days[len(days)-1].Date.Format("Jan 02"),
		days[0].Date.Format("Jan 02"))
	return fmt.Sprintf("  %s  %s  %s",
		accentStyle.Render(bars),
		dimStyle.Render(fmt.Sprintf("Σ $%.2f", total)),
		dimStyle.Render(rangeLabel))
}

// sparkBars renders values as a Unicode-block bar chart. Re-implemented
// here so the Usage view doesn't reach into Pulse's sparklineRow.
func sparkBars(values []float64, width int) string {
	if len(values) == 0 || width <= 0 {
		return ""
	}
	if len(values) > width {
		values = values[len(values)-width:]
	}
	max := 0.0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		return strings.Repeat("·", len(values))
	}
	const blocks = " ▁▂▃▄▅▆▇█"
	bs := []rune(blocks)
	var sb strings.Builder
	for _, v := range values {
		idx := int((v / max) * float64(len(bs)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(bs) {
			idx = len(bs) - 1
		}
		sb.WriteRune(bs[idx])
	}
	return sb.String()
}
