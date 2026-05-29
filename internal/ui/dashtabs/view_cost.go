package dashtabs

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
)

type costView struct{}

func newCostView() *costView                         { return &costView{} }
func (v *costView) Title() string                    { return "Cost" }
func (v *costView) ShortKey() string                 { return "9" }
func (v *costView) Init() tea.Cmd                    { return nil }
func (v *costView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

func (v *costView) Render(width, height int, snap Snapshot, _ bool) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Cost — cloud spend, first-class"))
	b.WriteString("\n\n")

	// Headline block: current rate, target, projections.
	b.WriteString(costHeadline(snap))
	b.WriteString("\n\n")

	// Spend-rate sparkline (reuses Pulse history).
	b.WriteString(titleStyle.Render("Spend rate ($/hr)"))
	b.WriteString("\n")
	b.WriteString(sparklineRow("$/hr      ", snap.History.SpendPerHr, width-16, "$%.2f"))
	b.WriteString("\n\n")

	// Top spenders (24h).
	b.WriteString(titleStyle.Render("Top spenders (last 24h)"))
	b.WriteString("\n")
	if len(snap.ProjectSpend24h) == 0 {
		b.WriteString(dimStyle.Render("  (no spend recorded in last 24h)"))
	} else {
		max := 0.0
		for _, r := range snap.ProjectSpend24h {
			if r.SpentUSD > max {
				max = r.SpentUSD
			}
		}
		for i, r := range snap.ProjectSpend24h {
			if i >= 8 {
				break
			}
			b.WriteString(projectSpendRow(r, max, width-30))
			b.WriteString("\n")
		}
	}

	// Per-provider breakdown (live).
	b.WriteString("\n")
	b.WriteString(titleStyle.Render("Live providers"))
	b.WriteString("\n")
	if len(snap.LiveInstances) == 0 {
		b.WriteString(dimStyle.Render("  (no live cloud instances)"))
	} else {
		byProv := map[string]float64{}
		byProvN := map[string]int{}
		for _, l := range snap.LiveInstances {
			if l == nil {
				continue
			}
			byProv[l.Provider] += float64(l.CostPerHourCents) / 100.0
			byProvN[l.Provider]++
		}
		for prov, rate := range byProv {
			b.WriteString(fmt.Sprintf("  %s · %d inst · %s\n",
				accentStyle.Render(prov),
				byProvN[prov],
				runningStyle.Render(fmt.Sprintf("$%.2f/hr", rate))))
		}
	}

	// LLM API spend (Anthropic). OpenRouter is not yet wired; those rows
	// will appear here once that path records usage.
	b.WriteString("\n")
	b.WriteString(titleStyle.Render("LLM API spend · last 24h"))
	b.WriteString("\n")
	if len(snap.LLMUsage24h) == 0 {
		b.WriteString(dimStyle.Render("  (no recorded calls — Anthropic is wired; OpenRouter is not yet)"))
	} else {
		var total float64
		for _, u := range snap.LLMUsage24h {
			total += u.USD()
		}
		b.WriteString(fmt.Sprintf("  %s   total\n", accentStyle.Render(fmt.Sprintf("$%.4f", total))))
		for _, u := range snap.LLMUsage24h {
			b.WriteString(fmt.Sprintf("    %s %-9s %s  %d calls\n",
				accentStyle.Render(fmt.Sprintf("$%6.4f", u.USD())),
				u.Feature,
				dimStyle.Render(shortStr(u.Model, 24)),
				u.Calls))
		}
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("  (See Usage tab for per-feature token breakdowns and 30-day spend.)"))
	b.WriteString("\n")

	_ = height
	return b.String()
}

func costHeadline(snap Snapshot) string {
	rate := snap.SpendUSDPerHour
	target := snap.SpendTargetUSD
	var rateStr string
	switch {
	case target > 0:
		rateStr = fmt.Sprintf("$%.2f/hr  (target $%.2f/hr)", rate, target)
	default:
		rateStr = fmt.Sprintf("$%.2f/hr", rate)
	}
	style := runningStyle
	if target > 0 && rate > target {
		style = failedStyle
	}
	daily := fmt.Sprintf("$%.2f/day at current rate", rate*24)
	monthly := fmt.Sprintf("$%.0f/mo at current rate", rate*24*30)

	return fmt.Sprintf("  %s\n  %s · %s",
		style.Render(rateStr),
		dimStyle.Render(daily),
		dimStyle.Render(monthly))
}

func projectSpendRow(p db.ProjectSpend, max float64, barW int) string {
	if barW < 12 {
		barW = 12
	}
	w := 0
	if max > 0 {
		w = int(float64(barW) * (p.SpentUSD / max))
	}
	if w < 0 {
		w = 0
	}
	if w > barW {
		w = barW
	}
	bar := runningStyle.Render(strings.Repeat("█", w)) + dimStyle.Render(strings.Repeat("░", barW-w))
	return fmt.Sprintf("  %-24s %s  %s",
		shortStr(p.Project, 24),
		accentStyle.Render(fmt.Sprintf("$%6.2f", p.SpentUSD)),
		bar)
}

// Attention: warn when spend exceeds target.
func (v *costView) Attention(snap Snapshot) Attention {
	if snap.SpendTargetUSD > 0 && snap.SpendUSDPerHour > snap.SpendTargetUSD {
		return AttentionWarn
	}
	return AttentionNone
}
