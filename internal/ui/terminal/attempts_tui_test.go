package terminal

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestAttemptsListHeaderUsesCanonicalJobID(t *testing.T) {
	model := attemptsListModel{
		jobID: 1692,
		job:   &db.Job{ID: 1692, Status: db.StatusQueued, Project: "role-encoding-injection"},
	}

	header := model.renderHeader()
	if !strings.Contains(header, "Attempts for job wj1692") {
		t.Fatalf("header = %q, want canonical job ID", header)
	}
	if strings.Contains(header, "job #1692") {
		t.Fatalf("header = %q, should not use raw numeric job ID", header)
	}
	if strings.Contains(header, "wi1692") {
		t.Fatalf("header = %q, should not use instance ID prefix for jobs", header)
	}
}

func TestAttemptsListTableShowsMachineAndProvider(t *testing.T) {
	model := attemptsListModel{
		views: []attemptView{
			{
				attempt: db.JobAttempt{AttemptNumber: 1, Status: db.StatusCanceled},
				launch:  &db.Launch{ID: 2736, Provider: "vastai", MachineID: "23779", ResolvedGPUName: "A100 PCIE", GPUMemGB: 80},
				phase:   phaseEnded,
				when:    1_778_342_787,
			},
		},
	}

	model.sessions = groupAttemptSessions(model.views)
	table := model.renderTable(0)
	if !strings.Contains(table, "Where") {
		t.Fatalf("table = %q, want Where column", table)
	}
	if !strings.Contains(table, "23779 @ Vast.ai") {
		t.Fatalf("table = %q, want '23779 @ Vast.ai'", table)
	}
}

func TestGroupAttemptSessionsFoldsSameHostRun(t *testing.T) {
	// 5 attempts newest-first: three on wi2704 (with one queued no-host
	// nested in the middle), then a different host, then another.
	views := []attemptView{
		{attempt: db.JobAttempt{AttemptNumber: 96, Host: "wi2704", Status: db.StatusRunning}},
		{attempt: db.JobAttempt{AttemptNumber: 95, Host: "", Status: db.StatusQueued}},
		{attempt: db.JobAttempt{AttemptNumber: 94, Host: "wi2704", Status: db.StatusCanceled, CloudOutcome: db.AttemptOutcomeSuperseded}},
		{attempt: db.JobAttempt{AttemptNumber: 13, Host: "wi2676", Status: db.StatusCanceled, CloudOutcome: db.AttemptOutcomeSuperseded}},
		{attempt: db.JobAttempt{AttemptNumber: 12, Host: "wi2678", Status: db.StatusCanceled, CloudOutcome: db.AttemptOutcomeOrphaned}},
	}
	sessions := groupAttemptSessions(views)
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3: %+v", len(sessions), sessions)
	}
	if got, want := len(sessions[0].indices), 3; got != want {
		t.Fatalf("first session has %d attempts, want %d", got, want)
	}
	if sessions[0].host != "wi2704" {
		t.Fatalf("first session host = %q, want wi2704", sessions[0].host)
	}
	if got, want := sessionRangeLabel(views, sessions[0]), "94–96"; got != want {
		t.Fatalf("range label = %q, want %q", got, want)
	}
	out := sessionOutcomeText(views, sessions[0])
	if !strings.Contains(out, "superseded") {
		t.Fatalf("session outcome = %q, want a superseded count", out)
	}
}

func TestAttemptsListCollapsedDefaultRendersOneRowPerSession(t *testing.T) {
	views := []attemptView{
		{attempt: db.JobAttempt{AttemptNumber: 3, Host: "wi2704", Status: db.StatusRunning}, phase: phaseRunning, when: 1_778_000_000},
		{attempt: db.JobAttempt{AttemptNumber: 2, Host: "wi2704", Status: db.StatusCanceled, CloudOutcome: db.AttemptOutcomeSuperseded}, phase: phaseEnded, when: 1_777_999_900},
		{attempt: db.JobAttempt{AttemptNumber: 1, Host: "wi2704", Status: db.StatusCanceled, CloudOutcome: db.AttemptOutcomeSuperseded}, phase: phaseEnded, when: 1_777_999_800},
	}
	model := attemptsListModel{
		views:    views,
		sessions: groupAttemptSessions(views),
	}
	table := model.renderTable(20)
	if !strings.Contains(table, "1–3") {
		t.Fatalf("collapsed table missing range label: %q", table)
	}
	// Expanded mode should show all three attempt numbers as their own rows.
	expanded := model.toggleExpanded()
	expandedTable := expanded.renderTable(20)
	for _, n := range []string{"\n1 ", "\n2 ", "\n3 "} {
		if !strings.Contains(expandedTable, n) {
			t.Fatalf("expanded table missing %q: %q", n, expandedTable)
		}
	}
	if strings.Contains(expandedTable, "1–3") {
		t.Fatalf("expanded table should not show session range, got %q", expandedTable)
	}
}

func TestRenderPlacementChainHighlightsBouncePattern(t *testing.T) {
	// Source position wi2734 (machine 52305) reappears across non-adjacent
	// sessions, interleaved with one-shot orphaned target attempts.
	views := []attemptView{
		{attempt: db.JobAttempt{AttemptNumber: 7, Host: "wi2734"}, launch: &db.Launch{MachineID: "52305"}},
		{attempt: db.JobAttempt{AttemptNumber: 6, Host: "wi2737", CloudOutcome: db.AttemptOutcomeOrphaned}},
		{attempt: db.JobAttempt{AttemptNumber: 5, Host: "wi2734"}, launch: &db.Launch{MachineID: "52305"}},
		{attempt: db.JobAttempt{AttemptNumber: 4, Host: "wi2738", CloudOutcome: db.AttemptOutcomeOrphaned}},
		{attempt: db.JobAttempt{AttemptNumber: 3, Host: "wi2734"}, launch: &db.Launch{MachineID: "52305"}},
		{attempt: db.JobAttempt{AttemptNumber: 2, Host: "wi2739", CloudOutcome: db.AttemptOutcomeOrphaned}},
		{attempt: db.JobAttempt{AttemptNumber: 1, Host: "wi2734"}, launch: &db.Launch{MachineID: "52305"}},
	}
	model := attemptsListModel{
		views:    views,
		sessions: groupAttemptSessions(views),
	}
	chain := model.renderPlacementChain(120)
	if !strings.Contains(chain, "wi2734") {
		t.Fatalf("chain missing stable host wi2734: %q", chain)
	}
	if !strings.Contains(chain, "52305") {
		t.Fatalf("chain missing stable machine id: %q", chain)
	}
	if !strings.Contains(chain, "×4") {
		t.Fatalf("chain missing repeat count: %q", chain)
	}
	for _, h := range []string{"wi2737", "wi2738", "wi2739"} {
		if !strings.Contains(chain, h) {
			t.Fatalf("chain missing transient host %s: %q", h, chain)
		}
	}
}

func TestRenderPlacementChainOmittedForSimpleHistories(t *testing.T) {
	// Two sessions on a single host; nothing to summarize.
	views := []attemptView{
		{attempt: db.JobAttempt{AttemptNumber: 2, Host: "wi2704"}},
		{attempt: db.JobAttempt{AttemptNumber: 1, Host: "wi2704"}},
	}
	model := attemptsListModel{
		views:    views,
		sessions: groupAttemptSessions(views),
	}
	if got := model.renderPlacementChain(120); got != "" {
		t.Fatalf("expected no chain summary, got %q", got)
	}
}

func TestVisibleWindowKeepsCursorInView(t *testing.T) {
	cases := []struct {
		cursor, total, height, wantStart, wantEnd int
	}{
		{0, 5, 10, 0, 5},       // fits
		{0, 100, 10, 0, 10},    // top
		{50, 100, 10, 45, 55},  // middle
		{99, 100, 10, 90, 100}, // bottom
	}
	for _, tc := range cases {
		s, e := visibleWindow(tc.cursor, tc.total, tc.height)
		if s != tc.wantStart || e != tc.wantEnd {
			t.Errorf("visibleWindow(%d,%d,%d) = (%d,%d); want (%d,%d)",
				tc.cursor, tc.total, tc.height, s, e, tc.wantStart, tc.wantEnd)
		}
	}
}
