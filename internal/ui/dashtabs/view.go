package dashtabs

import (
	tea "github.com/charmbracelet/bubbletea"
)

// View is a single tab in the dashboard. Each view owns its local UI state
// (cursor, expansion) but does no DB access — the parent passes a Snapshot
// at render time.
type View interface {
	Title() string
	ShortKey() string
	Init() tea.Cmd
	// Update may return a replacement View (typically itself, possibly with
	// updated local state). The parent never compares pointers, only values.
	Update(msg tea.Msg) (View, tea.Cmd)
	// Render produces the body of the view, occupying exactly width x height
	// terminal cells. Views must not assume any space beyond what is offered.
	Render(width, height int, snap Snapshot, focused bool) string
}

// Status is a marker each view may emit so the parent can decorate its tab
// label (e.g., highlight Alerts when there's something to act on).
type Attention int

const (
	AttentionNone Attention = iota
	AttentionInfo
	AttentionWarn
)

// AttentionReporter is an optional interface a view can implement to surface
// "this tab has something" without forcing the parent to know its internals.
type AttentionReporter interface {
	Attention(snap Snapshot) Attention
}
