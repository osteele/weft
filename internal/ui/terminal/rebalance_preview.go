package terminal

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
)

// rebalancePreviewModel renders the grouped-list rebalance preview modal.
type rebalancePreviewModel struct {
	active          bool
	loading         bool
	applying        bool
	moves           []orchestration.QueueRebalanceMove
	errMessage      string
	cursor          int
	progressMessage string
}

func (p *rebalancePreviewModel) reset() {
	p.active = false
	p.loading = false
	p.applying = false
	p.moves = nil
	p.errMessage = ""
	p.cursor = 0
	p.progressMessage = ""
}

func (p *rebalancePreviewModel) moveCursor(delta int) {
	if len(p.moves) == 0 {
		p.cursor = 0
		return
	}
	p.cursor += delta
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= len(p.moves) {
		p.cursor = len(p.moves) - 1
	}
}

var (
	rebalancePreviewTitleStyle      = movePickerTitleStyle
	rebalancePreviewDimStyle        = movePickerDimStyle
	rebalancePreviewHeaderStyle     = movePickerSectionStyle
	rebalanceOverBudgetStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Faint(true)
	rebalancePreviewErrorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	rebalancePreviewLoadingDotStyle = lipgloss.NewStyle().Foreground(tuiAccentColor).Bold(true)
)

func (p rebalancePreviewModel) View(width, height int) string {
	if !p.active {
		return ""
	}

	var b strings.Builder
	b.WriteString(rebalancePreviewTitleStyle.Render("Rebalance queued jobs"))
	b.WriteString("\n\n")

	switch {
	case p.loading:
		b.WriteString(rebalancePreviewLoadingDotStyle.Render("⠋"))
		b.WriteString(" Planning rebalance moves…")
		if msg := strings.TrimSpace(p.progressMessage); msg != "" {
			b.WriteString("\n  ")
			b.WriteString(rebalancePreviewDimStyle.Render(msg))
		}
	case p.applying:
		b.WriteString(rebalancePreviewLoadingDotStyle.Render("⠋"))
		b.WriteString(" Applying rebalance moves…")
		if msg := strings.TrimSpace(p.progressMessage); msg != "" {
			b.WriteString("\n  ")
			b.WriteString(rebalancePreviewDimStyle.Render(msg))
		}
	case strings.TrimSpace(p.errMessage) != "":
		b.WriteString(rebalancePreviewErrorStyle.Render("Rebalance failed: " + p.errMessage))
		b.WriteString("\n\n")
		b.WriteString(rebalancePreviewDimStyle.Render("[esc] dismiss"))
	case len(p.moves) == 0:
		b.WriteString("No rebalance moves found.")
		b.WriteString("\n\n")
		b.WriteString(rebalancePreviewDimStyle.Render("[esc] dismiss"))
	default:
		b.WriteString(rebalancePreviewHeaderStyle.Render("JOB        FROM → TO        RATIO   REASON"))
		b.WriteString("\n")
		visibleMoves, hidden := p.visibleMoves(max(1, height-10))
		for _, move := range visibleMoves {
			line := formatRebalanceMoveLine(move)
			if rebalanceMoveOverBudget(move) {
				line = rebalanceOverBudgetStyle.Render(line)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		if hidden > 0 {
			b.WriteString(rebalancePreviewDimStyle.Render(fmt.Sprintf("+%d more", hidden)))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(rebalancePreviewDimStyle.Render("[y] apply all  [n]/[esc] cancel"))
	}

	content := movePickerBorderStyle.Render(b.String())
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func (p rebalancePreviewModel) visibleMoves(limit int) ([]orchestration.QueueRebalanceMove, int) {
	if limit <= 0 || len(p.moves) <= limit {
		return p.moves, 0
	}
	start := p.cursor
	if start > len(p.moves)-limit {
		start = len(p.moves) - limit
	}
	if start < 0 {
		start = 0
	}
	return p.moves[start : start+limit], len(p.moves) - limit
}

func formatRebalanceMoveLine(move orchestration.QueueRebalanceMove) string {
	flag := ""
	if rebalanceMoveOverBudget(move) {
		flag = " over-budget"
	}
	return fmt.Sprintf("%-10s %-5s → %-5s  %.2f   %s%s",
		ids.FormatJobID(move.JobID),
		ids.FormatInstanceID(move.FromInstanceID),
		ids.FormatInstanceID(move.ToInstanceID),
		move.CostRatio,
		move.Reason,
		flag,
	)
}

func rebalanceMoveOverBudget(move orchestration.QueueRebalanceMove) bool {
	return move.CostCeiling > 0 && move.CostRatio > move.CostCeiling+1e-9
}

func countOverBudgetRebalanceMoves(moves []orchestration.QueueRebalanceMove) int {
	n := 0
	for _, move := range moves {
		if rebalanceMoveOverBudget(move) {
			n++
		}
	}
	return n
}
