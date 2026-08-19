// Package hit provides a screen plan for terminal UIs: the composed frame as
// an ordered list of physical rows (index == screen Y) with click targets
// attached to whole lines or to display-column spans within a line. The plan
// is view-agnostic — the target payload is whatever comparable type the
// consumer chooses; its zero value marks an inert line.
package hit

import "strings"

// Span is a column-scoped target within one line. Columns are DISPLAY columns
// (0-based, end exclusive), not byte offsets and not rune counts.
type Span[T comparable] struct {
	StartCol int
	EndCol   int
	Target   T
}

// Line is exactly one physical terminal row of the composed frame. Text is the
// final, width-truncated, already-styled string emitted verbatim.
type Line[T comparable] struct {
	Text       string
	RowIdx     int // the underlying list row the line draws; -1 for title/filler/status lines
	LineTarget T
	Spans      []Span[T]
}

// Plan is the whole frame. Index in Lines == screen Y. That identity is the entire
// point of this type: there is no other Y computation anywhere. Width and Height are the
// terminal dimensions the frame was composed for, and are the cache key for reusing it.
type Plan[T comparable] struct {
	Lines  []Line[T]
	Width  int
	Height int
}

// New returns an empty plan sized for a width x height terminal. The frame
// normally fills exactly height lines, so Lines is preallocated to that capacity.
func New[T comparable](width, height int) Plan[T] {
	return Plan[T]{Lines: make([]Line[T], 0, max(0, height)), Width: width, Height: height}
}

// Add appends one physical row. rowIdx names the underlying list row the line draws, or
// -1 for title, filler, and footer lines that stand for no row.
func (p *Plan[T]) Add(text string, rowIdx int, target T) {
	p.Lines = append(p.Lines, Line[T]{Text: text, RowIdx: rowIdx, LineTarget: target})
}

// AddSpan attaches a column-scoped target to the most recently added line.
func (p *Plan[T]) AddSpan(span Span[T]) {
	if len(p.Lines) == 0 {
		return
	}
	p.Lines[len(p.Lines)-1].Spans = append(p.Lines[len(p.Lines)-1].Spans, span)
}

func (p Plan[T]) Render() string {
	var b strings.Builder
	size := 0
	for _, line := range p.Lines {
		size += len(line.Text) + 1
	}
	b.Grow(size)
	for i, line := range p.Lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(line.Text)
	}
	return b.String()
}

// RowIdxAt returns the model row drawn at screen row y, or -1 when that line
// stands for no row (title, filler, footer) or y is off screen.
func (p Plan[T]) RowIdxAt(y int) int {
	if y < 0 || y >= len(p.Lines) {
		return -1
	}
	return p.Lines[y].RowIdx
}

// Hit resolves a press at display column x of screen row y. When spans overlap, the
// narrowest span containing x wins; otherwise the line-wide target applies. A line
// whose target is the zero value is inert.
func (p Plan[T]) Hit(x, y int) (T, bool) {
	var zero T
	if y < 0 || y >= len(p.Lines) {
		return zero, false
	}
	if x < 0 || x >= p.Width {
		return zero, false
	}
	line := p.Lines[y]
	best := -1
	bestWidth := 0
	for i, span := range line.Spans {
		if x < span.StartCol || x >= span.EndCol {
			continue
		}
		if w := span.EndCol - span.StartCol; best < 0 || w < bestWidth {
			best = i
			bestWidth = w
		}
	}
	if best >= 0 {
		return line.Spans[best].Target, true
	}
	if line.LineTarget != zero {
		return line.LineTarget, true
	}
	return zero, false
}
