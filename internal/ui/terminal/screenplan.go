package terminal

import "strings"

type targetKind uint8

const (
	targetNone targetKind = iota
	targetSelectRow
	targetToggleSection
	targetOpenURL
	targetRestartDaemon
	targetCopy
	targetCollapseBlocked
	targetIncidentJump
	targetAutoErrorToggle
	targetExpandAllBlockers
	targetDiagnose
	targetInstanceFailures
	targetInstances
	targetHosts
	targetDismissStatus
	targetControlsKey
)

type clickTarget struct {
	kind    targetKind
	rowIdx  int // index into m.groupedRows (grouped view) or m.jobs (flat view); -1 otherwise
	toggle  string
	url     string
	label   string
	payload string // copy payload when it is not derived from a grouped row
	jobID   int64  // job a disclosure-collapse target belongs to
	key     string // controls-line key token to invoke
}

func (t clickTarget) actionable() bool { return t.kind != targetNone }

// hitSpan is a column-scoped target within one line. Columns are DISPLAY columns
// (0-based, end exclusive), not byte offsets and not rune counts.
type hitSpan struct {
	startCol int
	endCol   int
	target   clickTarget
}

// screenLine is exactly one physical terminal row of the composed frame. text is the
// final, width-truncated, already-styled string emitted verbatim.
type screenLine struct {
	text       string
	rowIdx     int // grouped body rows only; -1 for title/filler/status lines
	lineTarget clickTarget
	spans      []hitSpan
}

// screenPlan is the whole frame. Index in lines == screen Y. That identity is the entire
// point of this type: there is no other Y computation anywhere. width and height are the
// terminal dimensions the frame was composed for, and are the cache key for reusing it.
type screenPlan struct {
	lines  []screenLine
	width  int
	height int
}

// newScreenPlan returns an empty plan sized for a width x height terminal. The frame
// normally fills exactly height lines, so lines is preallocated to that capacity.
func newScreenPlan(width, height int) screenPlan {
	return screenPlan{lines: make([]screenLine, 0, max(0, height)), width: width, height: height}
}

// add appends one physical row. rowIdx names the underlying list row the line draws, or
// -1 for title, filler, and footer lines that stand for no row.
func (p *screenPlan) add(text string, rowIdx int, target clickTarget) {
	p.lines = append(p.lines, screenLine{text: text, rowIdx: rowIdx, lineTarget: target})
}

// addSpan attaches a column-scoped target to the most recently added line.
func (p *screenPlan) addSpan(span hitSpan) {
	if len(p.lines) == 0 {
		return
	}
	p.lines[len(p.lines)-1].spans = append(p.lines[len(p.lines)-1].spans, span)
}

func (p screenPlan) render() string {
	var b strings.Builder
	size := 0
	for _, line := range p.lines {
		size += len(line.text) + 1
	}
	b.Grow(size)
	for i, line := range p.lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(line.text)
	}
	return b.String()
}

// hit resolves a press at display column x of screen row y. When spans overlap, the
// narrowest span containing x wins; otherwise the line-wide target applies.
func (p screenPlan) hit(x, y int) (clickTarget, bool) {
	if y < 0 || y >= len(p.lines) {
		return clickTarget{}, false
	}
	if x < 0 || x >= p.width {
		return clickTarget{}, false
	}
	line := p.lines[y]
	best := -1
	bestWidth := 0
	for i, span := range line.spans {
		if x < span.startCol || x >= span.endCol {
			continue
		}
		if w := span.endCol - span.startCol; best < 0 || w < bestWidth {
			best = i
			bestWidth = w
		}
	}
	if best >= 0 {
		return line.spans[best].target, true
	}
	if line.lineTarget.actionable() {
		return line.lineTarget, true
	}
	return clickTarget{}, false
}
