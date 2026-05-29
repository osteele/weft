package dashtabs

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// renderHeader is now just the tab strip. The weather line moved to the
// bottom of the screen — see renderWeatherLine, rendered as part of the
// three-line footer.
func renderHeader(width int, _ Snapshot, tabs []View, active int, attn []Attention, cycle CycleState, _ time.Time) string {
	if width <= 0 {
		return ""
	}
	return renderTabStrip(tabs, active, attn, cycle, width)
}

func renderWeatherLine(snap Snapshot, now time.Time) string {
	c := snap.Counts
	parts := []string{
		runningStyle.Render(fmt.Sprintf("● %d running", c.Running)),
		queuedStyle.Render(fmt.Sprintf("%d queued", c.Queued)),
		completedStyle.Render(fmt.Sprintf("%d done", c.Completed)),
	}
	if c.Failed > 0 {
		parts = append(parts, failedStyle.Render(fmt.Sprintf("%d failed", c.Failed)))
	}
	rate := snap.SpendUSDPerHour
	target := snap.SpendTargetUSD
	var rateStr string
	switch {
	case target > 0:
		rateStr = fmt.Sprintf("$%.2f/$%.2f/hr", rate, target)
	case rate > 0:
		rateStr = fmt.Sprintf("$%.2f/hr", rate)
	default:
		rateStr = "$0/hr"
	}
	rateStyle := dimStyle
	if target > 0 && rate > target {
		rateStyle = failedStyle
	}
	parts = append(parts, rateStyle.Render(rateStr))

	if len(snap.RecentFailures) > 0 {
		total := 0
		for _, f := range snap.RecentFailures {
			total += f.Count
		}
		parts = append(parts, failedStyle.Render(fmt.Sprintf("⚠ %d failures/24h", total)))
	}

	parts = append(parts, autopilotLabel(snap))

	return strings.Join(parts, "  ·  ")
}

func autopilotLabel(snap Snapshot) string {
	if snap.AutopilotState == nil {
		return dimStyle.Render("autopilot: unknown")
	}
	st := snap.AutopilotState
	if st.Paused {
		who := st.PausedBy
		if who == "" {
			who = "?"
		}
		reason := st.PausedReason
		if reason == "" {
			reason = "(no reason)"
		}
		return pausedStyle.Render(fmt.Sprintf("autopilot: PAUSED by %s — %s", who, reason))
	}
	if st.IsActive(time.Now(), 5*time.Minute) {
		return accentStyle.Render("autopilot: running pass")
	}
	return dimStyle.Render("autopilot: idle")
}

// tabInnerWidth returns the width of a tab's text label (without the
// 1+1 chars of horizontal padding that lipgloss adds via Padding(0, 1)).
func tabInnerWidth(t View) int {
	return len(fmt.Sprintf("%s %s", t.ShortKey(), t.Title()))
}

// tabLabelWidth returns the full rendered cell width — label plus the
// 2 chars of horizontal padding from the tab styles (Padding(0, 1)).
func tabLabelWidth(t View) int {
	return tabInnerWidth(t) + 2
}

// stylePadH is the per-side horizontal padding tab styles add. Keep in sync
// with tabActiveStyle / tabInactiveStyle / tabAttnStyle in styles.go.
const stylePadH = 1

// columnWidths returns the inner (unpadded) width of each visual column
// across the row layout. Column c's width is the max inner width of every
// tab assigned to position c on any row.
func columnWidths(tabs []View, rows [][]int) []int {
	ncols := 0
	for _, row := range rows {
		if len(row) > ncols {
			ncols = len(row)
		}
	}
	widths := make([]int, ncols)
	for _, row := range rows {
		for c, idx := range row {
			if w := tabInnerWidth(tabs[idx]); w > widths[c] {
				widths[c] = w
			}
		}
	}
	return widths
}

// rowRenderedWidth returns the visible width of one row given the shared
// column widths. Accounts for style padding (2 per cell) and the 1-char
// separator between cells.
func rowRenderedWidth(row []int, cols []int) int {
	w := 0
	for c := range row {
		if c > 0 {
			w++ // separator
		}
		w += cols[c] + 2*stylePadH
	}
	return w
}

// renderTabStrip lays out the tab strip with full labels, wrapping to as
// many rows as needed to fit `width`. Rows are split as evenly as possible
// (no orphans/widows): 10 tabs over 2 rows become 5+5, not 7+3.
func renderTabStrip(tabs []View, active int, attn []Attention, cycle CycleState, width int) string {
	// Right-side gauge first so we know how much room is left for labels.
	var right string
	if cycle.Enabled {
		right = accentStyle.Render(fmt.Sprintf("auto-rotate: %s", cycle.Interval))
	}
	rightW := lipgloss.Width(right)
	avail := width - rightW
	if rightW > 0 {
		avail -= 3 // gap between strip and gauge
	}
	if avail < 4 {
		avail = 4
	}

	rows := tabRowLayout(tabs, avail)
	cols := columnWidths(tabs, rows)
	// Render each row with column-aligned cells, then attach the right-side
	// gauge to the first row only (rows below it have nothing to anchor to).
	lines := make([]string, len(rows))
	for r, row := range rows {
		strip := renderTabRow(tabs, active, attn, row, cols)
		lines[r] = strip
	}
	// Right-align the gauge on the first row.
	if right != "" && len(lines) > 0 {
		gap := width - lipgloss.Width(lines[0]) - rightW
		if gap < 1 {
			gap = 1
		}
		lines[0] = lines[0] + strings.Repeat(" ", gap) + right
	}
	return strings.Join(lines, "\n")
}

// tabRowLayout computes how to split tabs across rows. It tries fewer rows
// first; once it finds a row count where the widest row (using column-aligned
// widths) fits, returns that even split.
func tabRowLayout(tabs []View, avail int) [][]int {
	n := len(tabs)
	if n == 0 {
		return nil
	}
	for nrows := 1; nrows <= n; nrows++ {
		rows := evenSplit(n, nrows)
		cols := columnWidths(tabs, rows)
		max := 0
		for _, row := range rows {
			if w := rowRenderedWidth(row, cols); w > max {
				max = w
			}
		}
		if max <= avail {
			return rows
		}
	}
	// Worst case: each tab on its own row.
	out := make([][]int, n)
	for i := range out {
		out[i] = []int{i}
	}
	return out
}

// evenSplit produces a row layout where each row has either base or base+1
// tabs. Extra tabs go to the leading rows, never to the trailing ones, so
// the last row is never sparser than the others by more than 1.
func evenSplit(n, nrows int) [][]int {
	if nrows < 1 {
		nrows = 1
	}
	base := n / nrows
	extra := n % nrows
	rows := make([][]int, nrows)
	idx := 0
	for r := 0; r < nrows; r++ {
		size := base
		if r < extra {
			size++
		}
		row := make([]int, 0, size)
		for j := 0; j < size; j++ {
			row = append(row, idx)
			idx++
		}
		rows[r] = row
	}
	return rows
}

func renderTabRow(tabs []View, active int, attn []Attention, row []int, cols []int) string {
	chunks := make([]string, 0, len(row))
	for c, i := range row {
		t := tabs[i]
		label := fmt.Sprintf("%s %s", t.ShortKey(), t.Title())
		// Pad the label to its column's max width so cells in different
		// rows but the same column index occupy the same horizontal span.
		if c < len(cols) {
			label = padRight(label, cols[c])
		}
		var style lipgloss.Style
		switch {
		case i == active:
			style = tabActiveStyle
		case i < len(attn) && attn[i] == AttentionWarn:
			style = tabAttnStyle
		default:
			style = tabInactiveStyle
		}
		chunks = append(chunks, style.Render(label))
	}
	return strings.Join(chunks, " ")
}

// rowOf returns the (row, col) of a tab index in a row layout.
func rowOf(rows [][]int, idx int) (int, int) {
	for r, row := range rows {
		for c, i := range row {
			if i == idx {
				return r, c
			}
		}
	}
	return 0, 0
}

// tabCenters returns the absolute column (within its row) of each tab's
// midpoint, given the shared column widths used at render time. Up/down
// navigation uses these to pick the tab whose column-center is closest.
//
// With column-aligned rendering, tabs at the same column index across rows
// produce the same center value — so ↑/↓ feels like moving in a grid.
func tabCenters(tabs []View, rows [][]int) map[int]int {
	cols := columnWidths(tabs, rows)
	centers := map[int]int{}
	for _, row := range rows {
		x := 0
		for c, idx := range row {
			cellW := cols[c] + 2*stylePadH
			centers[idx] = x + cellW/2
			x += cellW
			if c < len(row)-1 {
				x++ // separator
			}
		}
	}
	return centers
}

// neighborInRow returns the tab index step positions to the right (or left
// if step is negative) of `from` on the same row, wrapping at row ends.
func neighborInRow(rows [][]int, from, step int) int {
	r, c := rowOf(rows, from)
	row := rows[r]
	c = (c + step + len(row)) % len(row)
	return row[c]
}

// neighborInAdjacentRow returns the tab index whose horizontal center is
// closest to `from`'s center in the row `rowStep` away (wrapping around the
// top/bottom edge).
func neighborInAdjacentRow(tabs []View, rows [][]int, from, rowStep int) int {
	if len(rows) <= 1 {
		return from
	}
	r, _ := rowOf(rows, from)
	target := (r + rowStep + len(rows)) % len(rows)
	centers := tabCenters(tabs, rows)
	fromX := centers[from]
	best := rows[target][0]
	bestDist := abs(centers[best] - fromX)
	for _, idx := range rows[target][1:] {
		d := abs(centers[idx] - fromX)
		if d < bestDist {
			bestDist = d
			best = idx
		}
	}
	return best
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// renderStatusLine produces the middle of the three bottom-bar lines. It
// shows snapshot freshness, loading state, any transient status message,
// and the auto-rotate state when off (since the tab strip only shows
// auto-rotate when it's on).
func renderStatusLine(snap Snapshot, cycle CycleState, loading bool, transient string, transientStyleAccent bool) string {
	parts := []string{}
	if loading && snap.LoadedAt.IsZero() {
		parts = append(parts, queuedStyle.Render("loading…"))
	} else {
		ts := snap.LoadedAt.Format("15:04:05")
		if snap.LoadedAt.IsZero() {
			ts = "—"
		}
		label := "loaded " + ts
		if loading {
			label += " (refreshing)"
		}
		parts = append(parts, dimStyle.Render(label))
	}
	if !cycle.Enabled {
		parts = append(parts, dimStyle.Render("auto-rotate: off"))
	}
	if transient != "" {
		if transientStyleAccent {
			parts = append(parts, accentStyle.Render(transient))
		} else {
			parts = append(parts, dimStyle.Render(transient))
		}
	}
	return statusLineStyle.Render(strings.Join(parts, "   ·   "))
}

// renderHelpLine produces the lower bottom-bar line: per-tab keybindings.
// Keys are styled like uj's footer: `key:action` separated by two spaces.
func renderHelpLine(_ Snapshot) string {
	pairs := []string{
		"1-9,0:tab",
		"←/→:row",
		"↑/↓:rows",
		"Tab:linear",
		"c:auto-rotate",
		"p:autopilot",
		"r:refresh",
		"?:help",
		"q:quit",
	}
	return dimStyle.Render(strings.Join(pairs, "  "))
}
