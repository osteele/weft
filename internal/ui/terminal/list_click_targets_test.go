package terminal

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func clickTargetTestModel(jobs []*db.Job) listTUIModel {
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           96,
		height:          30,
		jobs:            jobs,
	}
	m.rebuildGroupedRows()
	return m
}

// planYForText returns the screen row of the first plan line containing
// substr, or fails the test.
func planYForText(t *testing.T, plan screenPlan, substr string) int {
	t.Helper()
	for y, line := range plan.lines {
		if strings.Contains(stripANSI(line.text), substr) {
			return y
		}
	}
	t.Fatalf("no plan line contains %q:\n%s", substr, plan.render())
	return -1
}

func hitKind(plan screenPlan, x, y int) (clickTarget, bool) {
	return plan.hit(x, y)
}

// glyphSpanFixture renders one ⌂ (inventory) row and one ☁ (interruptible
// rental) row and returns the model with the plan built for it.
func glyphSpanFixture(t *testing.T) (listTUIModel, screenPlan) {
	t.Helper()
	launchID := int64(7001)
	m := clickTargetTestModel([]*db.Job{
		{ID: 505, Status: db.StatusRunning, LaunchID: &launchID, Tags: []string{db.TagRental, db.TagPreemptible}, Description: "spot job"},
		{ID: 504, Status: db.StatusCompleted, Host: "cool30", Description: "inventory job"},
	})
	plan := m.buildGroupedScreenPlan()
	frame := stripANSI(plan.render())
	if !strings.Contains(frame, "⌂ wj504") || !strings.Contains(frame, "☁ wj505") {
		t.Fatalf("fixture must render one ⌂ row and one ☁ row:\n%s", frame)
	}
	return m, plan
}

// idSpanColumns returns the display columns of the job ID within the plan
// line at y: the start column, and the end column (exclusive).
func idSpanColumns(t *testing.T, plan screenPlan, y int, id string) (start, end int) {
	t.Helper()
	plain := stripANSI(plan.lines[y].text)
	idx := strings.Index(plain, id)
	if idx < 0 {
		t.Fatalf("line %d has no %q: %q", y, id, plain)
	}
	start = lipgloss.Width(plain[:idx])
	return start, start + lipgloss.Width(id)
}

func TestJobIDCopySpanResolvesForAmbiguousGlyphs(t *testing.T) {
	_, plan := glyphSpanFixture(t)
	for _, id := range []string{"wj504", "wj505"} {
		y := planYForText(t, plan, id+" ")
		start, end := idSpanColumns(t, plan, y, id)

		// The span is anchored at column 0: the decoration left of the ID
		// copies too, so glyph-width drift can never push the ID outside it.
		target, ok := hitKind(plan, 0, y)
		if !ok || target.kind != targetCopy || target.payload != id {
			t.Fatalf("%s: hit(0) = %+v, %v; want copy of %s", id, target, ok, id)
		}
		target, ok = hitKind(plan, start, y)
		if !ok || target.kind != targetCopy || target.payload != id {
			t.Fatalf("%s: hit(id start) = %+v, %v; want copy of %s", id, target, ok, id)
		}
		// Two cells past the ID end is still the copy span: the padding keeps
		// a terminal that draws ⌂/☁ wide from turning a copy into a select.
		target, ok = hitKind(plan, end+1, y)
		if !ok || target.kind != targetCopy || target.payload != id {
			t.Fatalf("%s: hit(id end + 1) = %+v, %v; want copy of %s", id, target, ok, id)
		}
		// Past the padding, the row is back to its select target.
		target, ok = hitKind(plan, end+2, y)
		if !ok || target.kind != targetSelectRow {
			t.Fatalf("%s: hit(id end + 2) = %+v, %v; want select", id, target, ok)
		}
	}
}

func TestJobIDCopySpanSurvivesSelectionPadding(t *testing.T) {
	// renderSelectedRow pads on the right only; were it to pad (or otherwise
	// shift content) on the left, a span recorded against the unselected row
	// would point at the wrong columns under selection.
	row := "- ⌂ wj504 — — x"
	want := row + strings.Repeat(" ", 20-lipgloss.Width(row))
	if got := stripANSI(renderSelectedRow(row, 20)); got != want {
		t.Fatalf("renderSelectedRow padding = %q, want right-side padding only (%q)", got, want)
	}

	m, plan := glyphSpanFixture(t)
	y := planYForText(t, plan, "wj504 ")
	start, end := idSpanColumns(t, plan, y, "wj504")

	// Select the row and confirm the span resolves at the same columns.
	rowIdx := plan.lines[y].rowIdx
	m.selectGroupedRowByIndex(rowIdx)
	selectedPlan := m.buildGroupedScreenPlan()
	sy := planYForText(t, selectedPlan, "wj504 ")
	if got := lipgloss.Width(selectedPlan.lines[sy].text); got != m.width {
		t.Fatalf("selected row display width = %d, want padded to %d", got, m.width)
	}
	selStart, selEnd := idSpanColumns(t, selectedPlan, sy, "wj504")
	if selStart != start || selEnd != end {
		t.Fatalf("selection moved the ID span: unselected [%d,%d), selected [%d,%d)", start, end, selStart, selEnd)
	}
	for _, p := range []screenPlan{plan, selectedPlan} {
		py := planYForText(t, p, "wj504 ")
		if target, ok := hitKind(p, start, py); !ok || target.kind != targetCopy || target.payload != "wj504" {
			t.Fatalf("hit(id start) = %+v, %v; want copy", target, ok)
		}
		if target, ok := hitKind(p, end+1, py); !ok || target.kind != targetCopy {
			t.Fatalf("hit(id end + 1) = %+v, %v; want copy under padding", target, ok)
		}
		if target, ok := hitKind(p, end+2, py); !ok || target.kind != targetSelectRow {
			t.Fatalf("hit(id end + 2) = %+v, %v; want select under padding", target, ok)
		}
	}
}

func TestSectionHeaderEnterTogglesCollapse(t *testing.T) {
	m := clickTargetTestModel([]*db.Job{
		{ID: 101, Status: db.StatusQueued, Description: "one"},
		{ID: 102, Status: db.StatusQueued, Description: "two"},
	})
	// groupedRows[0] is the section header; select it and press enter.
	m.selectGroupedRowByIndex(0)
	if section := m.selectedCollapsibleSection(); section == "" {
		t.Fatal("expected the cursor to sit on a collapsible section header")
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := next.(listTUIModel)
	if len(got.collapsedSections) != 1 {
		t.Fatalf("enter on a header should collapse its section, collapsed = %v", got.collapsedSections)
	}
	for _, row := range got.groupedRows {
		if row.job != nil {
			t.Fatalf("collapsed section still shows job row: %+v", row)
		}
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got = next.(listTUIModel)
	if len(got.collapsedSections) != 0 {
		t.Fatalf("second enter should re-expand the section, collapsed = %v", got.collapsedSections)
	}
}

func TestDisclosureDetailClickCollapsesParent(t *testing.T) {
	job := &db.Job{
		ID:     42,
		Status: db.StatusQueued,
		PlacementBlockedJSON: (&blockreason.Structured{
			Summary: "no compatible hosts",
			Launch:  "credit exhausted",
			Reuse: []blockreason.ReuseRejection{
				{Instance: "wi1", Reason: "disk insufficient"},
			},
		}).Marshal(),
	}
	m := clickTargetTestModel([]*db.Job{job})
	m.expandedBlocked = map[int64]bool{job.ID: true}
	m.rebuildGroupedRows()
	plan := m.buildGroupedScreenPlan()

	y := planYForText(t, plan, "reuse wi1")
	target, ok := hitKind(plan, 10, y)
	if !ok || target.kind != targetCollapseBlocked || target.jobID != job.ID {
		t.Fatalf("hit on disclosure detail = %+v, %v; want collapse of job %d", target, ok, job.ID)
	}
	m.dispatchTarget(target)
	if m.expandedBlocked[job.ID] {
		t.Fatal("clicking a disclosure detail row should collapse the parent breakdown")
	}
}

func TestSharedLaunchBlockerRowCopiesReasonAndIDs(t *testing.T) {
	seams := swapClipboardSeams(t, nil)
	mkJob := func(id int64) *db.Job {
		return &db.Job{
			ID:     id,
			Status: db.StatusQueued,
			PlacementBlockedJSON: (&blockreason.Structured{
				Summary: "no rental headroom; running instances couldn't accept this job",
				Launch:  "no rental headroom",
				Reuse:   []blockreason.ReuseRejection{{Instance: "wi1", Reason: "disk insufficient"}},
			}).Marshal(),
		}
	}
	m := clickTargetTestModel([]*db.Job{mkJob(750), mkJob(751)})
	plan := m.buildGroupedScreenPlan()

	y := planYForText(t, plan, "launch blocked for all: no rental headroom")
	target, ok := hitKind(plan, 4, y)
	if !ok || target.kind != targetCopy {
		t.Fatalf("hit on shared launch blocker = %+v, %v; want copy", target, ok)
	}
	cmd := m.dispatchTarget(target)
	if cmd == nil {
		t.Fatal("dispatchTarget returned no command")
	}
	if _, isCopy := cmd().(clipboardCopiedMsg); !isCopy {
		t.Fatal("dispatch should run a copy command")
	}
	want := "launch blocked for all: no rental headroom\n" + ids.FormatJobIDListCompact([]int64{750, 751})
	if len(seams.nativePayloads) != 1 || seams.nativePayloads[0] != want {
		t.Fatalf("payload = %q, want %q", seams.nativePayloads, want)
	}
}

func TestIncidentRollupClickJumpsToSampleJob(t *testing.T) {
	mkJob := func(id int64, launch, fingerprint string) *db.Job {
		return &db.Job{
			ID:     id,
			Status: db.StatusQueued,
			PlacementBlockedJSON: (&blockreason.Structured{
				Launch:      launch,
				Fingerprint: fingerprint,
				Reuse:       []blockreason.ReuseRejection{{Instance: "wi1", Reason: "disk insufficient"}},
			}).Marshal(),
		}
	}
	// 750 and 751 share a fingerprint (an incident); 752's differs, so no
	// section hoist replaces the rollup.
	m := clickTargetTestModel([]*db.Job{
		mkJob(750, "planner: search offers gpu_ram>=82", "vastai/search-offers/empty-result:no-offers"),
		mkJob(751, "planner: search offers gpu_ram>=10", "vastai/search-offers/empty-result:no-offers"),
		mkJob(752, "planner: search offers gpu=H100", "vastai/search-offers/empty-result:vram"),
	})
	plan := m.buildGroupedScreenPlan()

	y := planYForText(t, plan, "incident:")
	target, ok := hitKind(plan, 4, y)
	if !ok || target.kind != targetIncidentJump {
		t.Fatalf("hit on incident rollup = %+v, %v; want incident jump", target, ok)
	}
	m.dispatchTarget(target)
	if job := m.selectedGroupedJob(); job == nil || job.ID != 750 {
		t.Fatalf("incident click should select the sample job wj750, got %+v", job)
	}
	if !m.expandedBlocked[750] {
		t.Fatal("incident click should expand the sample job's blocker detail")
	}
	if !viewportContains(m.groupedViewportRows(), m.selectedGroupedRow()) {
		t.Fatal("incident click should scroll the sample job into view")
	}
}

func TestSelectedDetailPlanLineTargets(t *testing.T) {
	launchID := int64(7001)
	failedLaunchID := int64(7002)

	t.Run("job line diagnoses", func(t *testing.T) {
		m := clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusQueued, Description: "x"}})
		_, target := m.selectedDetailPlanLine("Job: wj1 · queued")
		if target.kind != targetDiagnose {
			t.Fatalf("Job: line target = %+v, want diagnose", target)
		}
	})

	t.Run("inventory host line travels to hosts", func(t *testing.T) {
		m := clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusRunning, Host: "cool30"}})
		line, target := m.selectedDetailPlanLine("Host: cool30")
		if target.kind != targetHosts || !strings.Contains(line, "(H:hosts)") {
			t.Fatalf("inventory Host: line = %q, %+v; want hosts target with hint", line, target)
		}
	})

	t.Run("rental host line travels to instances", func(t *testing.T) {
		m := clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusRunning, LaunchID: &launchID}})
		m.launchByID = map[int64]*db.Launch{launchID: {ID: launchID, Status: db.LaunchStatusRunning}}
		line, target := m.selectedDetailPlanLine("Host: wi7001 @ Vast.ai")
		if target.kind != targetInstances || !strings.Contains(line, "(i:instances)") {
			t.Fatalf("rental Host: line = %q, %+v; want instances target with hint", line, target)
		}
	})

	t.Run("failed rental host line opens failures only when the overlay has content", func(t *testing.T) {
		newModel := func() listTUIModel {
			m := clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusFailed, LaunchID: &failedLaunchID}})
			m.launchByID = map[int64]*db.Launch{failedLaunchID: {ID: failedLaunchID, Status: db.LaunchStatusFailed}}
			return m
		}
		line, target := newModel().selectedDetailPlanLine("Host: wi7002 · failed")
		if target.actionable() || strings.Contains(line, "(f:diagnose)") {
			t.Fatalf("failed Host: line without recent failures = %q, %+v; want inert and unhinted", line, target)
		}
		m := newModel()
		m.recentFailedInstances = &recentFailedInstances{
			items: []*db.Launch{{ID: failedLaunchID, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonInfraFailure}},
		}
		line, target = m.selectedDetailPlanLine("Host: wi7002 · failed")
		if target.kind != targetInstanceFailures || !strings.Contains(line, "(f:diagnose)") {
			t.Fatalf("failed Host: line = %q, %+v; want failures target with hint", line, target)
		}
	})

	t.Run("skypilot host line opens the dashboard", func(t *testing.T) {
		m := clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusRunning, Backend: db.BackendSkyPilot}})
		line, target := m.selectedDetailPlanLine("Host: SkyPilot · job 9")
		if target.actionable() {
			t.Fatalf("SkyPilot Host: line without a binding must be inert, got %+v", target)
		}
		m.externalBindingByJobID = map[int64]*db.ExternalJobBinding{
			1: {DashboardURL: "https://console.skypilot.co/dashboard/clusters/my-cluster"},
		}
		line, target = m.selectedDetailPlanLine("Host: SkyPilot · job 9 · https://console.skypilot.co/dashboard/clusters/my-cluster")
		if target.kind != targetOpenURL || target.url != "https://console.skypilot.co/dashboard/clusters/my-cluster" {
			t.Fatalf("SkyPilot Host: line = %q, %+v; want dashboard URL target", line, target)
		}
	})

	t.Run("move line stays inert", func(t *testing.T) {
		m := clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusRunning, Host: "cool30"}})
		line, target := m.selectedDetailPlanLine("Move: wi6201 → wi6202 · pending 3m")
		if target.actionable() || line != "Move: wi6201 → wi6202 · pending 3m" {
			t.Fatalf("Move: line = %q, %+v; want inert and unhinted", line, target)
		}
	})
}

func TestJobDetailLineClickOpensDiagnosis(t *testing.T) {
	m := clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusRunning, Host: "cool30", Description: "x"}})
	plan := m.buildGroupedScreenPlan()
	y := planYForText(t, plan, "Job: wj1")
	target, ok := hitKind(plan, 2, y)
	if !ok || target.kind != targetDiagnose {
		t.Fatalf("hit on Job: line = %+v, %v; want diagnose", target, ok)
	}
	m.dispatchTarget(target)
	if !m.jobDiagnosisLoading {
		t.Fatal("Job: line click should start the diagnosis overlay load")
	}
}

func TestTransientStatusLineClickDismisses(t *testing.T) {
	m := clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusQueued, Description: "x"}})
	m.statusMessage = "Killing job #1..."
	plan := m.buildGroupedScreenPlan()
	y := planYForText(t, plan, "Killing job #1...")
	target, ok := hitKind(plan, 2, y)
	if !ok || target.kind != targetDismissStatus {
		t.Fatalf("hit on status line = %+v, %v; want dismiss", target, ok)
	}
	m.dispatchTarget(target)
	if m.statusMessage != "" || m.flash.Message != "" {
		t.Fatalf("dismiss should clear status and flash, got %q / %q", m.statusMessage, m.flash.Message)
	}
}

func TestInstanceHealthLineClickOpensFailures(t *testing.T) {
	m := clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusQueued, Description: "x"}})
	m.recentFailedInstances = &recentFailedInstances{
		items: []*db.Launch{{ID: 9001, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonInfraFailure}},
	}
	plan := m.buildGroupedScreenPlan()
	y := planYForText(t, plan, "Launch failures:")
	target, ok := hitKind(plan, 2, y)
	if !ok || target.kind != targetInstanceFailures {
		t.Fatalf("hit on instance-health line = %+v, %v; want failures overlay", target, ok)
	}
	m.dispatchTarget(target)
	if !m.showInstanceFailures {
		t.Fatal("instance-health click should open the failures overlay")
	}
}

func TestAutopilotLineClickNeverTogglesAutopilot(t *testing.T) {
	newModel := func() listTUIModel {
		return clickTargetTestModel([]*db.Job{{ID: 1, Status: db.StatusQueued, Description: "x"}})
	}

	t.Run("monitoring line is inert", func(t *testing.T) {
		m := newModel()
		plan := m.buildGroupedScreenPlan()
		y := planYForText(t, plan, "Auto-pilot:")
		if target, ok := hitKind(plan, 2, y); ok && target.actionable() {
			t.Fatalf("monitoring Auto-pilot line must be inert, got %+v", target)
		}
	})

	t.Run("error line shows details without touching autopilot state", func(t *testing.T) {
		m := newModel()
		m.autoPersistentError = "offer search failed"
		m.lastAutoPilotErrorRaw = "offer search failed: exit 1"
		plan := m.buildGroupedScreenPlan()
		y := planYForText(t, plan, "Auto-pilot: failed")
		if !strings.Contains(stripANSI(plan.lines[y].text), "(e:details)") {
			t.Fatalf("error line should carry the click hint, got %q", stripANSI(plan.lines[y].text))
		}
		target, ok := hitKind(plan, 2, y)
		if !ok || target.kind != targetAutoErrorToggle {
			t.Fatalf("hit on error line = %+v, %v; want error-details toggle", target, ok)
		}
		m.dispatchTarget(target)
		if !m.showAutoPilotErrorDetails {
			t.Fatal("error line click should show the error details block")
		}
		if m.autopilotPaused {
			t.Fatal("no click target may toggle the autopilot")
		}
	})

	t.Run("blocked line expands blockers without touching autopilot state", func(t *testing.T) {
		m := newModel()
		m.autoBlockDetail = map[int64]*blockreason.Structured{
			1: {Launch: "no rental headroom", Reuse: []blockreason.ReuseRejection{{Instance: "wi1", Reason: "x"}}},
		}
		plan := m.buildGroupedScreenPlan()
		y := planYForText(t, plan, "Auto-pilot:")
		target, ok := hitKind(plan, 2, y)
		if !ok || target.kind != targetExpandAllBlockers {
			t.Fatalf("hit on blocked line = %+v, %v; want expand-all", target, ok)
		}
		m.dispatchTarget(target)
		if !m.expandedBlocked[1] {
			t.Fatal("blocked line click should expand all blockers")
		}
		if m.autopilotPaused {
			t.Fatal("no click target may toggle the autopilot")
		}
	})
}

func TestControlsLineClickTargets(t *testing.T) {
	m := clickTargetTestModel([]*db.Job{{ID: 301, Status: db.StatusQueued, Description: "queued"}})
	plan := m.buildGroupedScreenPlan()
	y := len(plan.lines) - 1
	text := stripANSI(plan.lines[y].text)
	if !strings.Contains(text, "x:kill/cancel") {
		t.Fatalf("controls line should offer kill/cancel for the selected queued job: %q", text)
	}

	// The kill/cancel token is keyboard-only: it fires immediately with no
	// confirmation, so a click on it resolves to nothing.
	killX := strings.Index(text, "x:kill/cancel")
	if target, ok := hitKind(plan, killX+1, y); ok && target.actionable() {
		t.Fatalf("kill token click must be inert, got %+v", target)
	}
	if cmd := m.dispatchTarget(clickTarget{kind: targetControlsKey, key: "x"}); cmd != nil {
		t.Fatal("dispatching the kill key via click must not run")
	}
	if strings.Contains(m.statusMessage, "Killing") {
		t.Fatalf("kill token click must not kill, statusMessage = %q", m.statusMessage)
	}
	// The autopilot toggle is keyboard-only everywhere, controls line included.
	autoX := strings.Index(text, "A:auto")
	if target, ok := hitKind(plan, autoX+1, y); ok && target.actionable() {
		t.Fatalf("autopilot token click must be inert, got %+v", target)
	}
	// A launch token spends money; it stays keyboard-only too.
	if launchX := strings.Index(text, "n:new instance"); launchX >= 0 {
		if target, ok := hitKind(plan, launchX+1, y); ok && target.actionable() {
			t.Fatalf("launch token click must be inert, got %+v", target)
		}
	}

	// Benign tokens resolve to their key's handler.
	for _, token := range []string{"r:refresh", "z:diagnose"} {
		x := strings.Index(text, token)
		if x < 0 {
			t.Fatalf("controls line missing %q: %q", token, text)
		}
		target, ok := hitKind(plan, x+1, y)
		if !ok || target.kind != targetControlsKey || target.key != token[:1] {
			t.Fatalf("hit on %q = %+v, %v; want controls-key target", token, target, ok)
		}
	}
}
