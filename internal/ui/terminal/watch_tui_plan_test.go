package terminal

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// watchPlanFixture returns a system-watch model with one rental instance (two
// jobs), one on-prem job, and one unplaced job — every selectable section
// populated.
func watchPlanFixture() watchModel {
	cloudInstance := &db.Launch{
		ID:       5,
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	}
	return watchModel{
		mode:           watchModeSystem,
		width:          120,
		height:         40,
		cloudInstances: []*db.Launch{cloudInstance},
		instanceIDs:    []int64{5},
		updates: map[int64]campaign.InstanceUpdate{
			5: {
				Launch: cloudInstance,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning, Project: "EXP-ALPHA", Description: "train model"},
					{ID: 89, Status: db.StatusQueued, Project: "EXP-DELTA", Description: "eval model"},
				},
			},
		},
		onPremHosts: []onPremHostSummary{
			{Name: "cool30", Jobs: []*db.Job{{ID: 41, Status: db.StatusRunning, Host: "cool30", Project: "BETA", Description: "eval model"}}},
		},
		unplacedJobs:   []*db.Job{{ID: 123, Status: db.StatusQueued, Project: "GAMMA", Description: "benchmark", GPUClass: "A100"}},
		jobProgressHWM: map[int64]int{},
	}
}

func TestWatchMouseClickSelectsRow(t *testing.T) {
	m := watchPlanFixture()
	plan := m.currentWatchPlan()

	// The unplaced job is the last selectable row: one instance header, two
	// cloud jobs, one on-prem job, then the unplaced job (index 4).
	y := planYForText(t, plan, "#123")
	next, cmd := m.Update(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 10, Y: y})
	got := next.(watchModel)
	if cmd != nil {
		t.Fatalf("row selection returned a command: %v", cmd)
	}
	if got.cursor != 4 {
		t.Fatalf("cursor = %d after click on unplaced row, want 4", got.cursor)
	}
	if job := got.selectedUnplacedJob(); job == nil || job.ID != 123 {
		t.Fatalf("selectedUnplacedJob = %+v, want #123", job)
	}
}

func TestWatchInstanceIDCopySpan(t *testing.T) {
	m := watchPlanFixture()
	plan := m.currentWatchPlan()
	id := ids.FormatInstanceID(5)
	y := planYForText(t, plan, "Instance "+id)
	start, end := idSpanColumns(t, plan, y, id)

	// Anchored at column 0 like the list TUI's job ID span: the decoration
	// left of the ID copies too, so glyph-width drift can never push the ID
	// outside the span.
	target, ok := plan.Hit(0, y)
	if !ok || target.kind != targetCopy || target.payload != id {
		t.Fatalf("Hit(0) = %+v, %v; want copy of %s", target, ok, id)
	}
	target, ok = plan.Hit(start, y)
	if !ok || target.kind != targetCopy || target.payload != id {
		t.Fatalf("Hit(id start) = %+v, %v; want copy of %s", target, ok, id)
	}
	// Two cells past the ID end is still the copy span: the padding keeps a
	// terminal that draws a preceding glyph wide from turning a copy into a
	// select.
	target, ok = plan.Hit(end+1, y)
	if !ok || target.kind != targetCopy || target.payload != id {
		t.Fatalf("Hit(id end + 1) = %+v, %v; want copy of %s", target, ok, id)
	}
	// Past the padding, the row is back to its select target.
	target, ok = plan.Hit(end+2, y)
	if !ok || target.kind != targetSelectRow {
		t.Fatalf("Hit(id end + 2) = %+v, %v; want select", target, ok)
	}
}

func TestWatchMouseClickCopiesInstanceID(t *testing.T) {
	m := watchPlanFixture()
	plan := m.currentWatchPlan()
	id := ids.FormatInstanceID(5)
	y := planYForText(t, plan, "Instance "+id)
	start, _ := idSpanColumns(t, plan, y, id)

	_, cmd := m.Update(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: start, Y: y})
	if cmd == nil {
		t.Fatal("click on instance ID returned no command")
	}
	msg, ok := cmd().(clipboardCopiedMsg)
	if !ok {
		t.Fatalf("copy command returned %T, want clipboardCopiedMsg", cmd())
	}
	if msg.label != id {
		t.Fatalf("copy label = %q, want %q", msg.label, id)
	}

	// The completion message surfaces through the flash, as in the list TUI.
	next, flashCmd := m.Update(msg)
	got := next.(watchModel)
	if got.flash.Message == "" {
		t.Fatal("flash empty after clipboardCopiedMsg")
	}
	if flashCmd == nil {
		t.Fatal("flash.Set should schedule expiry")
	}
}

func TestWatchMouseClickDaemonWarningRestartsDaemon(t *testing.T) {
	withStoppedDaemonStatus(t)
	calls := 0
	oldRestart := listRestartDaemonFunc
	listRestartDaemonFunc = func() (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
		calls++
		return daemoncontrol.Status{PID: 4242, Live: true}, daemoncontrol.EnsureRestarted, nil
	}
	t.Cleanup(func() { listRestartDaemonFunc = oldRestart })

	m := watchPlanFixture()
	plan := m.currentWatchPlan()
	y := planYForText(t, plan, "Daemon: stopped; click to start")

	_, cmd := m.Update(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 0, Y: y})
	if cmd == nil {
		t.Fatal("click on daemon warning returned no command")
	}
	msg, ok := cmd().(listDaemonRestartedMsg)
	if !ok {
		t.Fatalf("restart command returned %T, want listDaemonRestartedMsg", cmd())
	}
	if calls != 1 {
		t.Fatalf("restart calls = %d, want 1", calls)
	}

	next, _ := m.Update(msg)
	got := next.(watchModel)
	if got.flash.Message != "Daemon restarted (PID 4242)" {
		t.Fatalf("flash = %q, want restart success", got.flash.Message)
	}
}

// TestWatchClickNeverMutates pins the click rule: kill, terminate, unplace,
// retry, and the other mutating actions stay keyboard-only, so no line of the
// frame may carry a target outside the select/copy/navigation set — least of
// all the controls line, whose text names those verbs.
func TestWatchClickNeverMutates(t *testing.T) {
	m := watchPlanFixture()
	plan := m.currentWatchPlan()

	allowed := map[targetKind]bool{
		targetSelectRow:     true,
		targetCopy:          true,
		targetRestartDaemon: true,
		targetOpenURL:       true,
		targetInstances:     true,
		targetHosts:         true,
	}
	for y, line := range plan.Lines {
		if line.LineTarget.kind != targetNone && !allowed[line.LineTarget.kind] {
			t.Fatalf("line %d carries target kind %d outside the click-rule set", y, line.LineTarget.kind)
		}
		for _, span := range line.Spans {
			if !allowed[span.Target.kind] {
				t.Fatalf("line %d span carries target kind %d outside the click-rule set", y, span.Target.kind)
			}
		}
	}

	// The controls line names kill and terminate; clicking those tokens must
	// resolve to nothing.
	y := planYForText(t, plan, "[x] kill")
	plain := stripANSI(plan.Lines[y].Text)
	for _, token := range []string{"[x] kill", "[t] terminate", "[u] unplace"} {
		x := lipgloss.Width(plain[:strings.Index(plain, token)]) + 1
		if target, ok := plan.Hit(x, y); ok {
			t.Fatalf("click on %q resolved to %+v; mutating actions must stay keyboard-only", token, target)
		}
	}

	// And an actual press there changes nothing and issues no command.
	before := m.cursor
	next, cmd := m.Update(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 2, Y: y})
	if cmd != nil {
		t.Fatalf("click on controls line returned a command: %v", cmd)
	}
	if got := next.(watchModel).cursor; got != before {
		t.Fatalf("cursor moved on controls-line click: %d -> %d", before, got)
	}
}

func TestWatchMouseWheelStillScrolls(t *testing.T) {
	m := watchPlanFixture()
	next, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if got := next.(watchModel).cursor; got != 3 {
		t.Fatalf("cursor = %d after wheel down, want 3", got)
	}
	next, _ = next.(watchModel).Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if got := next.(watchModel).cursor; got != 0 {
		t.Fatalf("cursor = %d after wheel up, want 0", got)
	}
}

func TestWatchProjectClickSelectsRow(t *testing.T) {
	m := watchModel{
		mode:   watchModeProject,
		width:  100,
		height: 30,
		projectLines: []string{
			"proj-a (1 running, 1 recent/24h)",
			"  Running",
			"    #11 running train model",
			"proj-b (1 queued, 1 recent/24h)",
			"  Queued",
			"    #22 queued eval model",
		},
	}
	plan := m.currentWatchPlan()
	y := planYForText(t, plan, "#22")
	next, cmd := m.Update(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 6, Y: y})
	got := next.(watchModel)
	if cmd != nil {
		t.Fatalf("project row selection returned a command: %v", cmd)
	}
	if got.cursor != 5 {
		t.Fatalf("cursor = %d after click, want 5", got.cursor)
	}
}
