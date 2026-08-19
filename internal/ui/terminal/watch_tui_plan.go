package terminal

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/osteele/weft/internal/ids"
)

// watchFrameLine is one line of a watch view before it is committed to the
// screen plan: the final styled text plus the click target and column spans
// the line carries. rowIdx is the selectable row index for selectable lines,
// -1 otherwise.
type watchFrameLine struct {
	text   string
	rowIdx int
	target clickTarget
	spans  []hitSpan
}

func watchPlainLine(text string) watchFrameLine {
	return watchFrameLine{text: text, rowIdx: -1}
}

// watchPlanFromLines commits frame lines to a screen plan: index in the plan
// == screen Y.
func watchPlanFromLines(width, height int, lines []watchFrameLine) screenPlan {
	plan := newScreenPlan(width, height)
	for _, fl := range lines {
		plan.Add(fl.text, fl.rowIdx, fl.target)
		for _, span := range fl.spans {
			plan.AddSpan(span)
		}
	}
	return plan
}

// watchPlanFromText commits an already-rendered string to a plan with no
// targets, for views that do not offer click actions (help).
func watchPlanFromText(width, height int, s string) screenPlan {
	plan := newScreenPlan(width, height)
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		plan.Add(line, -1, clickTarget{})
	}
	return plan
}

// cacheWatchPlan records the plan a View() just composed so hit testing
// resolves clicks against the frame the user actually saw. Models built as
// bare struct literals in tests have no cache and simply skip the record.
func (m watchModel) cacheWatchPlan(plan screenPlan) screenPlan {
	if m.planCache != nil {
		*m.planCache = plan
	}
	return plan
}

// currentWatchPlan returns the plan from the most recent View() when it was
// composed for the current terminal size, and rebuilds otherwise. Models
// built as bare struct literals in tests have no cache and always rebuild.
func (m watchModel) currentWatchPlan() screenPlan {
	if m.planCache != nil && m.planCache.Width == m.width && m.planCache.Height == m.height && len(m.planCache.Lines) > 0 {
		return *m.planCache
	}
	return m.buildWatchPlan()
}

// buildWatchPlan composes the frame the active watch mode displays as a
// screen plan.
func (m watchModel) buildWatchPlan() screenPlan {
	switch {
	case m.mode.isInstanceBased():
		lines, cursorLine := m.renderInstanceView()
		return watchPlanFromLines(m.width, m.height, m.applyViewportLines(lines, cursorLine))
	case m.mode == watchModeSystem:
		return watchPlanFromLines(m.width, m.height, m.renderSystemView())
	case m.mode == watchModeProject:
		if m.projectHelp {
			return watchPlanFromText(m.width, m.height, m.renderProjectHelpView())
		}
		return m.renderProjectPlan()
	}
	return newScreenPlan(m.width, m.height)
}

// instanceIDCopySpan locates the instance ID in a rendered instance header
// line and returns the click span that copies it. It follows jobIDCopySpan's
// rule: the span is anchored at column 0 with two cells of right padding, so
// wherever a glyph precedes the ID, a terminal that disagrees on the glyph's
// width shifts the ID within the span but never outside it — a copy can
// degrade into a select, never the reverse.
func instanceIDCopySpan(line string, instanceID int64, width int) (hitSpan, bool) {
	plain := ansi.Strip(line)
	id := ids.FormatInstanceID(instanceID)
	idx := strings.Index(plain, id)
	if idx < 0 {
		return hitSpan{}, false
	}
	endCol := lipgloss.Width(plain[:idx]) + lipgloss.Width(id) + 2
	if width > 0 && endCol > width {
		endCol = width
	}
	return hitSpan{StartCol: 0, EndCol: endCol, Target: clickTarget{kind: targetCopy, label: id, payload: id}}, true
}

// dispatchWatchTarget performs the action a resolved click target names. The
// click rules are the list TUI's: a click selects the row it lands on, copies
// an identifier from a region whose only content is that identifier, or takes
// the action a status line already advertises. It never kills, terminates,
// unplaces, retries, or otherwise mutates a job or instance — those stay
// keyboard-only.
func (m *watchModel) dispatchWatchTarget(t clickTarget) tea.Cmd {
	switch t.kind {
	case targetSelectRow:
		m.cursor = t.rowIdx
		m.clampCursor()
		if m.mode == watchModeProject {
			m.adjustProjectOffset()
		}
		return nil
	case targetCopy:
		if t.payload == "" {
			return nil
		}
		return copyToClipboardCmd(t.label, t.payload)
	case targetRestartDaemon:
		return restartDaemonListCmd()
	case targetOpenURL:
		return openURLListCmd(t.url)
	case targetInstances:
		if m.mode == watchModeSystem {
			return nil
		}
		return func() tea.Msg { return switchToSystemWatchMsg{} }
	default:
		return nil
	}
}

// handleMouse resolves wheel scrolling and left presses against the plan of
// the frame on screen.
func (m watchModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.moveCursor(-3)
		return m, nil
	case tea.MouseButtonWheelDown:
		m.moveCursor(3)
		return m, nil
	}
	if m.movePicker.active || m.projectHelp || m.autoRunRateInputActive {
		return m, nil
	}
	if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
		target, _ := m.currentWatchPlan().Hit(msg.X, msg.Y)
		return m, m.dispatchWatchTarget(target)
	}
	return m, nil
}
