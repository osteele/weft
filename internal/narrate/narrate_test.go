package narrate

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestDiffSnapshots_FirstTickIsAllAdds(t *testing.T) {
	next := &Snapshot{
		Time: time.Now(),
		Jobs: map[int64]JobView{
			1: {ID: 1, Status: "running"},
			2: {ID: 2, Status: "queued"},
		},
		Instances: map[int64]InstanceView{
			10: {ID: 10, Status: "running"},
		},
	}
	d := DiffSnapshots(nil, next)
	if len(d.JobAdded) != 2 {
		t.Fatalf("want 2 jobs added, got %d", len(d.JobAdded))
	}
	if len(d.InstAdded) != 1 {
		t.Fatalf("want 1 inst added, got %d", len(d.InstAdded))
	}
	if d.Empty() {
		t.Fatal("delta should not be empty when items added")
	}
}

func TestDiffSnapshots_StatusTransition(t *testing.T) {
	prev := &Snapshot{
		Jobs:      map[int64]JobView{1: {ID: 1, Status: "queued"}},
		Instances: map[int64]InstanceView{},
	}
	next := &Snapshot{
		Jobs:      map[int64]JobView{1: {ID: 1, Status: "running"}},
		Instances: map[int64]InstanceView{},
	}
	d := DiffSnapshots(prev, next)
	if len(d.JobChanged) != 1 {
		t.Fatalf("want 1 changed, got %d", len(d.JobChanged))
	}
	if d.JobChanged[0].Before.Status != "queued" || d.JobChanged[0].After.Status != "running" {
		t.Fatalf("unexpected change: %+v", d.JobChanged[0])
	}
}

func TestDiffSnapshots_NoChangeIsEmpty(t *testing.T) {
	prev := &Snapshot{
		Jobs:      map[int64]JobView{1: {ID: 1, Status: "running"}},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "idle"},
	}
	next := &Snapshot{
		Jobs:      map[int64]JobView{1: {ID: 1, Status: "running"}},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "idle"},
	}
	if !DiffSnapshots(prev, next).Empty() {
		t.Fatal("identical snapshots should produce empty delta")
	}
}

func TestDiffSnapshots_InstanceGraceTransition(t *testing.T) {
	deadline := int64(1_000_000_000)
	prev := &Snapshot{
		Instances: map[int64]InstanceView{10: {ID: 10, Status: "running"}},
		Jobs:      map[int64]JobView{},
	}
	next := &Snapshot{
		Instances: map[int64]InstanceView{10: {ID: 10, Status: "grace", GraceDeadline: &deadline}},
		Jobs:      map[int64]JobView{},
	}
	d := DiffSnapshots(prev, next)
	if len(d.InstChanged) != 1 {
		t.Fatalf("want 1 inst changed, got %d", len(d.InstChanged))
	}
	if d.InstChanged[0].Before.Status != "running" || d.InstChanged[0].After.Status != "grace" {
		t.Fatalf("unexpected change: %+v", d.InstChanged[0])
	}
}

func TestDiffSnapshots_RoutineAutopilotStateChangeIsEmpty(t *testing.T) {
	prev := &Snapshot{
		Jobs:      map[int64]JobView{},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "idle"},
	}
	next := &Snapshot{
		Jobs:      map[int64]JobView{},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "running"},
	}
	d := DiffSnapshots(prev, next)
	if !d.Empty() {
		t.Fatal("routine autopilot state change should be empty")
	}
}

func TestDiffSnapshots_AutopilotPauseChangeIsNotEmpty(t *testing.T) {
	prev := &Snapshot{
		Jobs:      map[int64]JobView{},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "idle"},
	}
	next := &Snapshot{
		Jobs:      map[int64]JobView{},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "paused", Paused: true},
	}
	if DiffSnapshots(prev, next).Empty() {
		t.Fatal("autopilot pause should produce non-empty delta")
	}
}

func TestSession_AppendRecap_TriggersCompactionAtThreshold(t *testing.T) {
	s, err := NewSession(SessionOptions{CompactionThreshold: 50})
	if err != nil {
		t.Fatal(err)
	}
	// First small recap: under threshold
	if shouldCompact := s.AppendRecap("short recap"); shouldCompact {
		t.Fatal("should not compact yet")
	}
	// Big recap pushes us over threshold (50 token approximation: ~200 chars)
	big := strings.Repeat("x", 250)
	if shouldCompact := s.AppendRecap(big); !shouldCompact {
		t.Fatal("should have triggered compaction")
	}
}

func TestSession_ReplaceWithCompacted_ResetsTokens(t *testing.T) {
	s, err := NewSession(SessionOptions{CompactionThreshold: 50})
	if err != nil {
		t.Fatal(err)
	}
	s.AppendRecap(strings.Repeat("x", 400))
	if len(s.Recaps()) != 1 {
		t.Fatalf("want 1 recap, got %d", len(s.Recaps()))
	}
	s.ReplaceWithCompacted("compact summary")
	if len(s.Recaps()) != 1 {
		t.Fatalf("compacted should leave one entry, got %d", len(s.Recaps()))
	}
	if s.Recaps()[0].Recap != "compact summary" {
		t.Fatalf("unexpected entry: %q", s.Recaps()[0].Recap)
	}
}

func TestFormatPriorRecap_EmptyAndNonEmpty(t *testing.T) {
	if FormatPriorRecap(nil) != "" {
		t.Fatal("nil entries should produce empty string")
	}
	out := FormatPriorRecap([]recapEntry{
		{At: time.Unix(1700000000, 0).UTC(), Recap: "alpha"},
		{At: time.Unix(1700000060, 0).UTC(), Recap: "beta"},
	})
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("expected both recaps in output: %q", out)
	}
	if !strings.Contains(out, "---") {
		t.Fatalf("expected separator between entries: %q", out)
	}
}

func TestFormatSnapshot_StableJSON(t *testing.T) {
	snap := &Snapshot{
		Time: time.Unix(1700000000, 0).UTC(),
		Jobs: map[int64]JobView{
			2: {ID: 2, Status: "running", Host: "cool30"},
			1: {ID: 1, Status: "queued"},
		},
		Autopilot: AutopilotView{State: "idle"},
	}
	out := FormatSnapshot(snap)
	idxOne := strings.Index(out, `"id":1`)
	idxTwo := strings.Index(out, `"id":2`)
	if idxOne < 0 || idxTwo < 0 || idxOne > idxTwo {
		t.Fatalf("jobs not sorted by id ascending:\n%s", out)
	}
}

func TestFormatSnapshot_IncludesRunningJobProgress(t *testing.T) {
	snap := &Snapshot{
		Time: time.Unix(1700000000, 0).UTC(),
		Jobs: map[int64]JobView{
			1858: {ID: 1858, Status: "running", Project: "llm-performance-models", ProgressPct: 91},
		},
	}

	out := FormatSnapshot(snap)
	if !strings.Contains(out, `"progress":"91%"`) {
		t.Fatalf("progress missing from snapshot:\n%s", out)
	}
}

func TestBuildSnapshot_LoadsLiveJobProgress(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, db.LaunchHost(launchID), "/tmp", "echo ok", "progress job")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	if _, err := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:         launchID,
		JobProgressID:    jobID,
		JobProgressPct:   91,
		JobProgressPhase: 1,
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}

	snap, err := BuildSnapshot(database, SnapshotOptions{})
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	job := snap.Jobs[jobID]
	if job.ProgressPct != 91 {
		t.Fatalf("ProgressPct = %d, want 91", job.ProgressPct)
	}
}

func TestFormatSnapshot_CuratesAutopilotRoutineState(t *testing.T) {
	snap := &Snapshot{
		Time:      time.Unix(1700000000, 0).UTC(),
		Jobs:      map[int64]JobView{},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{
			State:          "running",
			PassAgeSeconds: 300,
			LastSummary:    "placed 30 jobs",
		},
	}

	out := FormatSnapshot(snap)
	for _, forbidden := range []string{"autopilot", "pass_age_s", "last_summary", "placed 30 jobs"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("routine autopilot detail %q leaked into snapshot:\n%s", forbidden, out)
		}
	}
}

func TestFormatSnapshot_KeepsBlockingAutopilotState(t *testing.T) {
	snap := &Snapshot{
		Time:      time.Unix(1700000000, 0).UTC(),
		Jobs:      map[int64]JobView{},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{
			State:        "paused",
			Paused:       true,
			PausedReason: "manual pause",
		},
	}

	out := FormatSnapshot(snap)
	if !strings.Contains(out, `"autopilot"`) || !strings.Contains(out, `"paused_reason":"manual pause"`) {
		t.Fatalf("blocking autopilot state missing from snapshot:\n%s", out)
	}
}

func TestFormatSnapshot_OmitsStalePlacementReasonsForPlacedQueuedJobs(t *testing.T) {
	launchID := int64(2418)
	snap := &Snapshot{
		Time: time.Unix(1700000000, 0).UTC(),
		Jobs: map[int64]JobView{
			1: {
				ID:               1,
				Status:           "queued",
				Project:          "markov-attention",
				LaunchID:         &launchID,
				PlacementReasons: []string{"older infrastructure failures"},
			},
			2: {
				ID:               2,
				Status:           "queued",
				Project:          "structural-probes",
				PlacementReasons: []string{"no cloud providers available"},
			},
		},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "idle"},
	}

	out := FormatSnapshot(snap)
	if strings.Contains(out, "older infrastructure failures") {
		t.Fatalf("placed queued job leaked stale placement reason:\n%s", out)
	}
	if !strings.Contains(out, "no cloud providers available") {
		t.Fatalf("unplaced queued job lost current placement reason:\n%s", out)
	}
}

func TestBuildSnapshotExcludesProcessedJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	unprocessedID, err := db.RecordQueued(database, "", "/tmp/unprocessed", "echo ok", "unprocessed")
	if err != nil {
		t.Fatalf("record unprocessed: %v", err)
	}
	processedID, err := db.RecordQueued(database, "", "/tmp/processed", "echo ok", "processed")
	if err != nil {
		t.Fatalf("record processed: %v", err)
	}
	if err := db.AddJobTag(database, processedID, db.ProcessedTag); err != nil {
		t.Fatalf("tag processed: %v", err)
	}

	snap, err := BuildSnapshot(database, SnapshotOptions{})
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if _, ok := snap.Jobs[unprocessedID]; !ok {
		t.Fatalf("unprocessed job %d missing from snapshot: %#v", unprocessedID, snap.Jobs)
	}
	if _, ok := snap.Jobs[processedID]; ok {
		t.Fatalf("processed job %d leaked into snapshot: %#v", processedID, snap.Jobs)
	}
}

func TestFormatDelta_IncludesTerminatedInstanceAndPreviousInstance(t *testing.T) {
	launchID := int64(2418)
	delta := Delta{
		JobChanged: []JobChange{{
			Before: JobView{ID: 10, Status: "running", Project: "markov-attention", LaunchID: &launchID},
			After:  JobView{ID: 10, Status: "queued", Project: "markov-attention"},
		}},
		InstTerminated: []InstanceView{{
			ID:                launchID,
			Status:            "running",
			Provider:          "vastai",
			GPUSpec:           "RTX 4090",
			TerminationReason: "infra_failure",
			TerminationDetail: "agent heartbeat expired",
		}},
	}

	out := FormatDelta(delta)
	for _, want := range []string{
		`"prev_instance_id":2418`,
		`"instances_terminated"`,
		`"termination_reason":"infra_failure"`,
		`"termination_detail":"agent heartbeat expired"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q from delta:\n%s", want, out)
		}
	}
}

func TestFallbackNarrationSummarizesTerminalJobs(t *testing.T) {
	exitOne := 1
	got := FallbackNarration(Delta{
		JobFinished: []JobView{
			{ID: 1, Status: "completed"},
			{ID: 2, Status: "failed", ExitCode: &exitOne},
		},
		AutopilotOld: AutopilotView{State: "running"},
		AutopilotNew: AutopilotView{State: "idle"},
	})
	for _, want := range []string{"2 jobs finished", "1 completed", "1 failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("fallback narration missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "autopilot moved from running to idle") {
		t.Fatalf("fallback narration leaked routine autopilot transition: %q", got)
	}
}

func TestFormatLifecycleEventsAndFallback(t *testing.T) {
	events := []db.LifecycleEvent{
		{ID: 10, OccurredAt: 1710000000, EventKind: db.EventRelaunchLaunchSuccess, LaunchID: 42, GPUSpec: "RTX 4090", JobCount: 2},
		{ID: 11, OccurredAt: 1710000001, EventKind: db.EventReconcileGraceExpired, LaunchID: 42, Detail: "grace expired"},
	}
	formatted := FormatLifecycleEvents(events)
	for _, want := range []string{`"id":10`, `"kind":"relaunch.launch_success"`, `"instance_id":42`, `"detail":"grace expired"`} {
		if !strings.Contains(formatted, want) {
			t.Fatalf("formatted events missing %q: %s", want, formatted)
		}
	}
	fallback := FallbackEventNarration(events)
	for _, want := range []string{"1 reconcile.grace_expired event", "1 relaunch.launch_success event"} {
		if !strings.Contains(fallback, want) {
			t.Fatalf("fallback missing %q: %q", want, fallback)
		}
	}
}

func TestStatusLineHeaderJobGrammar(t *testing.T) {
	base := StatusLine{
		Now:            time.Unix(1700000000, 0).UTC(),
		AutopilotState: "idle",
	}
	cases := []struct {
		name string
		line StatusLine
		want string
	}{
		{
			name: "running and queued",
			line: func() StatusLine {
				sl := base
				sl.RunningJobs = 2
				sl.QueuedJobs = 30
				return sl
			}(),
			want: "2 jobs running | 30 queued",
		},
		{
			name: "single running",
			line: func() StatusLine {
				sl := base
				sl.RunningJobs = 1
				return sl
			}(),
			want: "1 job running",
		},
		{
			name: "queued first",
			line: func() StatusLine {
				sl := base
				sl.QueuedJobs = 30
				return sl
			}(),
			want: "30 jobs queued",
		},
		{
			name: "completed first",
			line: func() StatusLine {
				sl := base
				sl.UnprocessedCompleted = 3
				sl.CompletedProjects = []string{"weft", "augur"}
				return sl
			}(),
			want: "3 jobs completed (augur, weft)",
		},
		{
			name: "single failed first",
			line: func() StatusLine {
				sl := base
				sl.UnprocessedFailed = 1
				sl.FailedProjects = []string{"augur"}
				return sl
			}(),
			want: "1 job failed (augur)",
		},
		{
			name: "completed and failed",
			line: func() StatusLine {
				sl := base
				sl.UnprocessedCompleted = 2
				sl.UnprocessedFailed = 1
				sl.CompletedProjects = []string{"weft"}
				sl.FailedProjects = []string{"augur"}
				return sl
			}(),
			want: "2 jobs completed (weft) | 1 failed (augur)",
		},
		{
			name: "completed without projects",
			line: func() StatusLine {
				sl := base
				sl.UnprocessedCompleted = 1
				return sl
			}(),
			want: "1 job completed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := tc.line.HeaderLines(0)
			if len(lines) < 2 {
				t.Fatalf("expected job count line, got %v", lines)
			}
			if !strings.Contains(lines[1], tc.want) {
				t.Fatalf("HeaderLines()[1] = %q, want %q", lines[1], tc.want)
			}
		})
	}
}

func TestStatusLineHeaderDropsUnprocessedProjectParensBeforeActiveProjects(t *testing.T) {
	sl := StatusLine{
		Now:                  time.Unix(1700000000, 0).UTC(),
		RunningJobs:          3,
		UnprocessedCompleted: 5,
		UnprocessedFailed:    2,
		CompletedProjects:    []string{"augur", "weft"},
		FailedProjects:       []string{"augur"},
		Projects:             []string{"augur", "weft", "remote-jobs"},
		AutopilotState:       "idle",
	}

	lines := sl.HeaderLines(80)
	if len(lines) < 2 {
		t.Fatalf("expected job line, got %v", lines)
	}
	line := lines[1]
	if strings.Contains(line, "(augur") || strings.Contains(line, "(weft") {
		t.Fatalf("unprocessed project parens should be dropped before active projects are truncated:\n%s", line)
	}
	if !strings.Contains(line, "3 jobs running | 5 completed | 2 failed") {
		t.Fatalf("load-bearing counts missing:\n%s", line)
	}
	if !strings.Contains(strings.Join(lines, " "), "remote-jobs") {
		t.Fatalf("active project list should remain after dropping parens:\n%v", lines)
	}
}

func TestStatusLineHeaderMovesProjectListToNextLineBeforeOverCompacting(t *testing.T) {
	sl := StatusLine{
		Now:               time.Unix(1700000000, 0).UTC(),
		QueuedJobs:        24,
		UnprocessedFailed: 9,
		Projects:          []string{"llm-performance-mode", "markov-attention", "role-encoder-injection", "structural-probes"},
		AutopilotState:    "running",
	}

	lines := sl.HeaderLines(80)
	if len(lines) < 3 {
		t.Fatalf("expected project list on a separate line, got %v", lines)
	}
	if !strings.Contains(lines[1], "24 jobs queued | 9 failed") {
		t.Fatalf("job counters missing from second line: %q", lines[1])
	}
	if strings.Contains(lines[1], "llm-") || strings.Contains(lines[1], "markov") || strings.Contains(lines[1], "struct") {
		t.Fatalf("project list should not be forced inline with job counters: %q", lines[1])
	}
	joinedProjects := strings.Join(lines[2:], " ")
	for _, want := range []string{"llm-performance-mode", "markov-attention", "role-encoder-injection", "structural-probes"} {
		if !strings.Contains(joinedProjects, want) {
			t.Fatalf("project %q missing from separate project lines: %v", want, lines)
		}
	}
	for _, line := range lines {
		if len(line) > 80 {
			t.Fatalf("line width = %d, want <= 80: %q", len(line), line)
		}
	}
}
