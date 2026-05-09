package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

func TestRenderJobListGroupedStatusPlainSectionsAndOrder(t *testing.T) {
	launchID := int64(44)
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "cool30", Project: "proj", Description: "run a"},
		{ID: 2, Status: db.StatusStarting, Host: "cool30", Project: "proj", Description: "run b"},
		{ID: 3, Status: db.StatusQueued, Host: "cool30", Project: "proj", Description: "queue"},
		{ID: 10, Status: db.StatusPendingPlacement, Host: "", Project: "proj", Description: "needs placement"},
		{ID: 11, Status: db.StatusQueued, Host: "", Project: "proj", Description: "still unplaced"},
		{ID: 4, Status: db.StatusCompleted, ExitCode: testIntPtr(0), Host: "cool30", Project: "proj", Description: "ok"},
		{ID: 5, Status: db.StatusFailed, Host: "cool30", Project: "proj", Description: "failed"},
		{ID: 6, Status: db.StatusDead, Host: "cool30", Project: "proj", Description: "dead"},
		{ID: 7, Status: db.StatusCompleted, ExitCode: testIntPtr(2), Host: "cool30", Project: "proj", Description: "bad exit"},
		{ID: 8, Status: db.StatusKilled, Host: "cool30", Project: "proj", Description: "killed"},
		{ID: 9, Status: db.StatusCanceled, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "proj", Description: "canceled"},
	}

	out := renderJobListGroupedStatusPlain(jobs, 0)

	wantOrder := []string{
		"Running (2):",
		"Queued (1):",
		"Unplaced (2):",
		"Completed (1):",
		"Failed (3):",
		"Killed/Canceled (2):",
	}
	if strings.Contains(out, "Launching") {
		t.Fatalf("did not expect Launching section for pending-placement job with no launch, got:\n%s", out)
	}
	last := -1
	for _, marker := range wantOrder {
		idx := strings.Index(out, marker)
		if idx < 0 {
			t.Fatalf("missing section marker %q in output:\n%s", marker, out)
		}
		if idx <= last {
			t.Fatalf("section %q out of order in output:\n%s", marker, out)
		}
		last = idx
	}

	if !strings.Contains(out, "- ⌂ wj4 — proj ok — completed ok") {
		t.Fatalf("missing completion line in output:\n%s", out)
	}
	if !strings.Contains(out, "- ⌂ wj7 — proj bad exit — completed (exit 2)") {
		t.Fatalf("missing failed-completed line in output:\n%s", out)
	}
	if !strings.Contains(out, "-   wj9 — proj canceled — canceled") {
		t.Fatalf("missing rental canceled line in output:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainNone(t *testing.T) {
	out := renderJobListGroupedStatusPlain(nil, 0)
	if out != "None\n" {
		t.Fatalf("output = %q, want %q", out, "None\n")
	}
}

func TestRenderJobListGroupedStatusPlainAt_ShowsProgressAndTiming(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(99)
	jobs := []*db.Job{
		{
			ID:        42,
			Status:    db.StatusRunning,
			LaunchID:  &launchID,
			Project:   "proj",
			StartTime: 4_400,
			CreatedAt: 4_300,
			QueuedAt:  4_350,
			Command:   "python train.py",
		},
		{
			ID:          43,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "queued",
			CreatedAt:   4_000,
			QueuedAt:    4_200,
		},
		{
			ID:          44,
			Status:      db.StatusCompleted,
			Project:     "proj",
			Description: "done",
			CreatedAt:   3_200,
			EndTime:     testInt64Ptr(4_940),
			ExitCode:    testIntPtr(0),
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, map[int64]*db.LaunchLiveState{
		launchID: {LaunchID: launchID, JobProgressID: 42, JobProgressPct: 75},
	}, nil, nil, nil, nil, now)

	for _, want := range []string{
		"-   wj42 — proj python train.py — running 75% — ETA ~57m (20m–2h53) — started 10m ago",
		"-   wj43 — proj queued — created 16m ago",
		"-   wj44 — proj done — completed 1m ago — completed ok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestRenderJobListGroupedStatusPlainAt_RunningWithoutProgressDoesNotDuplicateRunningLabel(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(77)
	jobs := []*db.Job{
		{
			ID:        99,
			Status:    db.StatusRunning,
			LaunchID:  &launchID,
			Project:   "proj",
			StartTime: 4_400,
			Command:   "python worker.py",
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	want := "-   wj99 — proj python worker.py"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
	if !strings.Contains(out, "— started 10m ago") {
		t.Fatalf("missing started timing in output:\n%s", out)
	}
	if strings.Contains(out, " — running — ") {
		t.Fatalf("unexpected duplicate running label in output:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_RunningDayScaleTimingShowsDayAndHour(t *testing.T) {
	now := time.Unix(200_000, 0)
	jobs := []*db.Job{
		{
			ID:          199,
			Status:      db.StatusRunning,
			Host:        "cool30",
			Project:     "proj",
			Description: "long runner",
			StartTime:   now.Unix() - (42 * 3600), // 1d18h
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	want := "— started 1d18h ago"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_CompletionsFallbackToPlacedWhenEndMissing(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:          53,
			Status:      db.StatusCompleted,
			Project:     "proj",
			Description: "legacy completion",
			QueuedAt:    4_700,
			ExitCode:    testIntPtr(0),
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	want := "-   wj53 — proj legacy completion — placed 5m ago — completed ok"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_QueuedUsesHydratedPlacementTime(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(44)
	jobs := []*db.Job{
		{
			ID:          1693,
			Status:      db.StatusQueued,
			LaunchID:    &launchID,
			Project:     "proj",
			Description: "restored move",
			QueuedAt:    4_900,
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, map[int64]int64{1693: 4_000}, nil, now)
	want := "-   wj1693 — proj restored move — placed 16m ago"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_ShowsBlockedReason(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:                 50,
			Status:             db.StatusQueued,
			Project:            "proj",
			Description:        "waiting",
			QueuedAt:           4_700,
			QueueBlockedReason: "first retry budget exceeded: elapsed 1h2m >= limit 45m",
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	lineWant := "  -   wj50 — proj waiting — created 5m ago"
	if !strings.Contains(out, lineWant) {
		t.Fatalf("missing %q in output:\n%s", lineWant, out)
	}
	blockedWant := "  blocked: first retry budget exceeded: elapsed 1h2m >= limit 45m (1)"
	if !strings.Contains(out, blockedWant) {
		t.Fatalf("missing %q in output:\n%s", blockedWant, out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_GroupsUnplacedByBlockedReason(t *testing.T) {
	now := time.Unix(5_000, 0)
	reasonAmpere := "planner: no offers from providers for gpu=AMPERE+ vram>=26GB reliability>=0.95"
	reasonNvidia := "planner: no offers from providers for gpu=NVIDIA vram>=26GB reliability>=0.95"
	reason3090 := "planner: no offers from providers for gpu=3090 vram>=20GB reliability>=0.95"
	end := int64(4_820)
	mk := func(id int64, desc, reason string) *db.Job {
		return &db.Job{
			ID:                 id,
			Status:             db.StatusQueued,
			Project:            "proj",
			Description:        desc,
			QueuedAt:           4_000,
			EndTime:            &end,
			QueueBlockedReason: reason,
		}
	}
	jobs := []*db.Job{
		mk(1394, "role-encoding-injection EXP-050", reasonAmpere),
		mk(1392, "role-encoding-injection EXP-049", reasonAmpere),
		mk(1388, "head-type-ontology EXP-163", reasonNvidia),
		mk(1387, "structural-probes EXP-084 baseline", reason3090),
		mk(1386, "structural-probes EXP-084 anonymized", reason3090),
		mk(1383, "structural-probes EXP-085 c-command", reason3090),
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)

	// Subheaders present with correct counts, in first-seen order.
	ampereHead := "  blocked: " + reasonAmpere + " (2)"
	nvidiaHead := "  blocked: " + reasonNvidia + " (1)"
	hunk3090 := "  blocked: " + reason3090 + " (3)"
	for _, want := range []string{ampereHead, nvidiaHead, hunk3090} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing subheader %q in output:\n%s", want, out)
		}
	}
	if i1, i2, i3 := strings.Index(out, hunk3090), strings.Index(out, nvidiaHead), strings.Index(out, ampereHead); !(i1 < i2 && i2 < i3) {
		t.Fatalf("subheaders out of order in output:\n%s", out)
	}

	// Jobs are indented under their subheader, and the old per-job
	// "    blocked: ..." form is gone.
	for _, want := range []string{
		"  -   wj1394 — ",
		"  -   wj1392 — ",
		"  -   wj1388 — ",
		"  -   wj1387 — ",
		"  -   wj1386 — ",
		"  -   wj1383 — ",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing grouped job row %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "    blocked: ") {
		t.Fatalf("unexpected per-job blocked line survived:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_UnplacedWithoutBlockedReasonStaysUngrouped(t *testing.T) {
	now := time.Unix(5_000, 0)
	reason := "planner: no offers from providers for gpu=3090 vram>=20GB reliability>=0.95"
	jobs := []*db.Job{
		{
			ID:                 201,
			Status:             db.StatusQueued,
			Project:            "proj",
			Description:        "has reason",
			QueuedAt:           4_000,
			QueueBlockedReason: reason,
		},
		{
			ID:          202,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "no reason",
			QueuedAt:    4_000,
		},
	}
	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)

	headWant := "  blocked: " + reason + " (1)"
	if !strings.Contains(out, headWant) {
		t.Fatalf("missing subheader %q in output:\n%s", headWant, out)
	}
	// Grouped job is indented; ungrouped job uses the base indent.
	if !strings.Contains(out, "  -   wj201 — ") {
		t.Fatalf("missing indented grouped row for wj201 in output:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	foundUngrouped := false
	for _, line := range lines {
		if strings.HasPrefix(line, "-   wj202 — ") {
			foundUngrouped = true
			break
		}
	}
	if !foundUngrouped {
		t.Fatalf("expected ungrouped row starting with '-   wj202 — ', got:\n%s", out)
	}

	// Ungrouped row must follow the grouped subheader+job.
	if strings.Index(out, "-   wj202") < strings.Index(out, headWant) {
		t.Fatalf("ungrouped row should trail grouped output in:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_UnplacedShowsCreatedNotQueuedAge(t *testing.T) {
	// Each autopilot retry pass closes the open attempt and creates a new
	// one, which stamps queued_at to "now" via createAttemptTx. The
	// unplaced section must show the job's true age (CreatedAt), not the
	// freshly-reset QueuedAt.
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:          1561,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "blocked job",
			CreatedAt:   2_600,             // job was created 40m ago
			QueuedAt:    now.Unix() - 2*60, // last retry 2m ago
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	want := "-   wj1561 — proj blocked job — created 40m ago"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
	if strings.Contains(out, "queued 2m ago") {
		t.Fatalf("unplaced row should not show reset queued_at as the age:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_ShowsRetryPendingTimingForUnplacedRetry(t *testing.T) {
	now := time.Unix(5_000, 0)
	end := int64(4_940)
	jobs := []*db.Job{
		{
			ID:          51,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "retry me",
			QueuedAt:    4_000,
			EndTime:     &end,
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	want := "-   wj51 — proj retry me — retry pending 1m ago"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_ShowsRetryRejectedTimingForBudgetGate(t *testing.T) {
	now := time.Unix(5_000, 0)
	end := int64(4_940)
	jobs := []*db.Job{
		{
			ID:                 52,
			Status:             db.StatusQueued,
			Project:            "proj",
			Description:        "retry blocked",
			QueuedAt:           4_000,
			EndTime:            &end,
			QueueBlockedReason: "first retry budget exceeded: elapsed 1h2m >= limit 45m",
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	want := "-   wj52 — proj retry blocked — retry rejected 1m ago"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_ReclassifiesPendingPlacementWhenLaunchRunning(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(2468)
	jobs := []*db.Job{
		{
			ID:          70,
			Status:      db.StatusPendingPlacement,
			LaunchID:    &launchID,
			Project:     "proj",
			Description: "stale launch pending",
			QueuedAt:    4_700,
		},
	}

	out := renderJobListGroupedStatusPlainAt(
		jobs,
		0,
		nil,
		map[int64]string{launchID: db.LaunchStatusRunning},
		nil,
		nil,
		nil,
		now,
	)
	if !strings.Contains(out, "Queued (1):") {
		t.Fatalf("expected queued section, got:\n%s", out)
	}
	if strings.Contains(out, "Launching (1):") {
		t.Fatalf("did not expect launching section, got:\n%s", out)
	}
	if !strings.Contains(out, "wj70") || !strings.Contains(out, "— placed 5m ago") {
		t.Fatalf("missing queued timing row in output:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_AlignsTimingSuffixToRightEdge(t *testing.T) {
	now := time.Unix(5_000, 0)
	width := 100
	jobs := []*db.Job{
		{
			ID:          61,
			Status:      db.StatusRunning,
			Host:        "cool30",
			Project:     "short-proj",
			Description: "short desc",
			StartTime:   4_200,
		},
		{
			ID:          62,
			Status:      db.StatusRunning,
			Host:        "cool30",
			Project:     "very-long-project-name",
			Description: "this is a much longer description that would otherwise force ragged started timing text",
			StartTime:   4_200,
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, width, nil, nil, nil, nil, nil, now)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected section header + 2 rows, got:\n%s", out)
	}

	row1 := lines[1]
	row2 := lines[2]
	token := "— ETA "
	idx1 := strings.Index(row1, token)
	idx2 := strings.Index(row2, token)
	if idx1 < 0 || idx2 < 0 {
		t.Fatalf("expected both rows to contain ETA suffix:\n%s", out)
	}
	col1 := lipgloss.Width(row1[:idx1])
	col2 := lipgloss.Width(row2[:idx2])
	if col1 != col2 {
		t.Fatalf("suffix columns do not align: col1=%d col2=%d\n%s", col1, col2, out)
	}
	if lipgloss.Width(row1) != width || lipgloss.Width(row2) != width {
		t.Fatalf("row widths should fill terminal width=%d; got row1=%d row2=%d\n%s",
			width, lipgloss.Width(row1), lipgloss.Width(row2), out)
	}
}

// Jobs in pending_placement with no launch attached are unplaced, regardless
// of how recently they entered that state. The grouped-list detail line
// already labels these "unplaced" via TargetKind; the group classification
// must agree — otherwise the section header and the selected-job footer
// disagree (see wj1307-style reports).
func TestRenderJobListGroupedStatusPlainAt_PendingPlacementWithoutLaunchIsUnplaced(t *testing.T) {
	now := time.Unix(5_000, 0)
	pending := db.StatusPendingPlacement
	cases := []struct {
		name      string
		pendingAt *int64
	}{
		{"fresh (just submitted)", ptrInt64(now.Add(-30 * time.Second).Unix())},
		{"stale (long pending)", ptrInt64(now.Add(-10 * time.Minute).Unix())},
		{"no pending_at recorded", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs := []*db.Job{
				{
					ID:            71,
					Status:        db.StatusQueued,
					PendingStatus: &pending,
					PendingAt:     tc.pendingAt,
					Project:       "proj",
					Description:   "pending placement",
					QueuedAt:      4_700,
				},
			}
			out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
			if !strings.Contains(out, "Unplaced (1):") {
				t.Fatalf("expected unplaced section, got:\n%s", out)
			}
			if strings.Contains(out, "Launching") {
				t.Fatalf("did not expect launching section, got:\n%s", out)
			}
		})
	}
}

func ptrInt64(v int64) *int64 { return &v }

func TestRenderJobListGroupedStatusPlainAt_QueuedCloudJob_BucketsByLaunchStatus(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(5551)
	job := &db.Job{
		ID:          80,
		Status:      db.StatusQueued,
		LaunchID:    &launchID,
		Project:     "proj",
		Description: "cloud queued",
		QueuedAt:    4_900,
	}

	siblingRunning := &db.Job{
		ID:          99,
		Status:      db.StatusRunning,
		LaunchID:    &launchID,
		Project:     "proj",
		Description: "sibling running",
	}

	cases := []struct {
		name         string
		launchStatus string
		liveState    map[int64]*db.LaunchLiveState
		siblings     []*db.Job
		wantSection  string
	}{
		{"planned", db.LaunchStatusPlanned, nil, nil, "Launching (1):"},
		{"launching", db.LaunchStatusLaunching, nil, nil, "Launching (1):"},
		// Instance VM is up but no sibling is genuinely running: treat as
		// still launching (agent hasn't dispatched yet).
		{"running_no_active_job", db.LaunchStatusRunning, nil, nil, "Launching (1):"},
		// Stale JobProgressID must not matter — without a genuinely
		// running sibling we still bucket as launching.
		{
			"running_stale_progress_id",
			db.LaunchStatusRunning,
			map[int64]*db.LaunchLiveState{launchID: {LaunchID: launchID, JobProgressID: 99}},
			nil,
			"Launching (1):",
		},
		// The active job may be filtered out of this view. The live phase
		// still tells us the launch is running a job, so this job is queued
		// behind it rather than waiting for the rental to launch.
		{
			"running_live_phase_active",
			db.LaunchStatusRunning,
			map[int64]*db.LaunchLiveState{launchID: {LaunchID: launchID, InstancePhase: "running:99"}},
			nil,
			"Queued (1):",
		},
		// A sibling is actually Running on this launch, so this job is
		// queued behind it.
		{
			"running_sibling_active",
			db.LaunchStatusRunning,
			nil,
			[]*db.Job{siblingRunning},
			"Queued (1):",
		},
		{"failed", db.LaunchStatusFailed, nil, nil, "Unplaced (1):"},
		{"cancelled", db.LaunchStatusCancelled, nil, nil, "Unplaced (1):"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs := append([]*db.Job{job}, tc.siblings...)
			out := renderJobListGroupedStatusPlainAt(
				jobs,
				0,
				tc.liveState,
				map[int64]string{launchID: tc.launchStatus},
				nil,
				nil,
				nil,
				now,
			)
			if !strings.Contains(out, tc.wantSection) {
				t.Fatalf("expected %q for launch status %q, got:\n%s", tc.wantSection, tc.launchStatus, out)
			}
		})
	}
}

func TestRenderJobListGroupedStatusPlainAt_LaunchingShowsPhaseAndElapsed(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(2329)
	jobs := []*db.Job{
		{
			ID:          1725,
			Status:      db.StatusQueued,
			LaunchID:    &launchID,
			Project:     "proj",
			Description: "launching job",
			QueuedAt:    4_800,
		},
	}
	out := renderJobListGroupedStatusPlainWithOptions(jobs, 0, groupedStatusRenderOptions{
		launchLiveByID:   map[int64]*db.LaunchLiveState{launchID: {LaunchID: launchID, BootstrapStage: "deps_installing"}},
		launchStatusByID: map[int64]string{launchID: db.LaunchStatusLaunching},
		launchByID:       map[int64]*db.Launch{launchID: {ID: launchID, CreatedAt: 4_700}},
		now:              now,
	})
	want := "-   wj1725 — proj launching job — deps installing · 5m ago"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
	if strings.Contains(out, "instance wi2329 starting") {
		t.Fatalf("old launching suffix still rendered:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_LaunchingGroupsMultiJobInstance(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(2329)
	jobs := []*db.Job{
		{ID: 1734, Status: db.StatusQueued, LaunchID: &launchID, Project: "proj", Description: "second"},
		{ID: 1725, Status: db.StatusQueued, LaunchID: &launchID, Project: "proj", Description: "first"},
	}
	out := renderJobListGroupedStatusPlainWithOptions(jobs, 0, groupedStatusRenderOptions{
		launchLiveByID:   map[int64]*db.LaunchLiveState{launchID: {LaunchID: launchID, BootstrapStage: "image_pull"}},
		launchStatusByID: map[int64]string{launchID: db.LaunchStatusLaunching},
		launchByID:       map[int64]*db.Launch{launchID: {ID: launchID, CreatedAt: 4_866}},
		now:              now,
		launchSpinner:    "⠹",
		launchingETA: groupedStatusLaunchingETA{
			totalP50:     5 * time.Minute,
			totalSamples: 5,
		},
	})
	for _, want := range []string{
		"  ⠹ wi2329 — image pulling · 2m ago · ~2m remaining (2 jobs)",
		"  -   wj1725 — proj first",
		"  -   wj1734 — proj second",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	if strings.Index(out, "wj1725") > strings.Index(out, "wj1734") {
		t.Fatalf("jobs not sorted by ID in launch bucket:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_LaunchingReadyUsesCheckAndZeroRemaining(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(2332)
	jobs := []*db.Job{
		{ID: 1749, Status: db.StatusQueued, LaunchID: &launchID, Project: "proj", Description: "ready job"},
	}
	out := renderJobListGroupedStatusPlainWithOptions(jobs, 0, groupedStatusRenderOptions{
		launchLiveByID:   map[int64]*db.LaunchLiveState{launchID: {LaunchID: launchID, BootstrapStage: "ready"}},
		launchStatusByID: map[int64]string{launchID: db.LaunchStatusLaunching},
		launchByID:       map[int64]*db.Launch{launchID: {ID: launchID, CreatedAt: 4_000}},
		now:              now,
		launchSpinner:    "⠸",
		launchingETA: groupedStatusLaunchingETA{
			totalP50:     10 * time.Minute,
			totalSamples: 8,
		},
	})
	want := "- ✓ wj1749 — proj ready job — agent ready · 16m ago · ~0s remaining"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
	if strings.Contains(out, "overdue") {
		t.Fatalf("expected p50 overrun to stay advisory, got:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_LaunchingOverdueUsesBootstrapDeadline(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(2333)
	deadline := now.Add(-time.Second).Unix()
	jobs := []*db.Job{
		{ID: 1751, Status: db.StatusQueued, LaunchID: &launchID, Project: "proj", Description: "deadline job"},
	}
	out := renderJobListGroupedStatusPlainWithOptions(jobs, 0, groupedStatusRenderOptions{
		launchLiveByID:   map[int64]*db.LaunchLiveState{launchID: {LaunchID: launchID, BootstrapStage: "deps_installing"}},
		launchStatusByID: map[int64]string{launchID: db.LaunchStatusLaunching},
		launchByID: map[int64]*db.Launch{launchID: {
			ID:                    launchID,
			CreatedAt:             now.Add(-2 * time.Minute).Unix(),
			BootstrapDeadlineUnix: &deadline,
		}},
		now: now,
		launchingETA: groupedStatusLaunchingETA{
			totalP50:     10 * time.Minute,
			totalSamples: 8,
		},
	})
	if !strings.Contains(out, "overdue") {
		t.Fatalf("expected overdue once bootstrap deadline is exceeded:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_LaunchingFutureDeadlineSuppressesOverdue(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(2334)
	deadline := now.Add(10 * time.Minute).Unix()
	providerRunningAt := now.Add(-2 * time.Minute).Unix()
	jobs := []*db.Job{
		{ID: 1752, Status: db.StatusQueued, LaunchID: &launchID, Project: "proj", Description: "runpod job"},
	}
	out := renderJobListGroupedStatusPlainWithOptions(jobs, 0, groupedStatusRenderOptions{
		launchLiveByID:   map[int64]*db.LaunchLiveState{launchID: {LaunchID: launchID, BootstrapStage: "deps_installing"}},
		launchStatusByID: map[int64]string{launchID: db.LaunchStatusLaunching},
		launchByID: map[int64]*db.Launch{launchID: {
			ID:                    launchID,
			CreatedAt:             now.Add(-30 * time.Minute).Unix(),
			ProviderRunningAt:     &providerRunningAt,
			BootstrapDeadlineUnix: &deadline,
		}},
		now: now,
		launchingETA: groupedStatusLaunchingETA{
			totalP50:     time.Minute,
			totalSamples: 8,
		},
	})
	if strings.Contains(out, "overdue") {
		t.Fatalf("did not expect overdue before provider-scoped bootstrap deadline:\n%s", out)
	}
	if !strings.Contains(out, "2m ago") {
		t.Fatalf("expected launch elapsed to use bootstrap origin, got:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_LaunchingPrefersMatureStageETA(t *testing.T) {
	now := time.Unix(5_000_000, 0)
	launchID := int64(2340)
	jobs := []*db.Job{
		{ID: 1750, Status: db.StatusQueued, LaunchID: &launchID, Project: "proj", Description: "stage eta"},
	}
	out := renderJobListGroupedStatusPlainWithOptions(jobs, 0, groupedStatusRenderOptions{
		launchLiveByID:   map[int64]*db.LaunchLiveState{launchID: {LaunchID: launchID, BootstrapStage: "deps_installing"}},
		launchStatusByID: map[int64]string{launchID: db.LaunchStatusLaunching},
		launchByID:       map[int64]*db.Launch{launchID: {ID: launchID, CreatedAt: now.Add(-9 * time.Minute).Unix()}},
		now:              now,
		launchingETA: groupedStatusLaunchingETA{
			totalP50:     10 * time.Minute,
			totalSamples: 8,
			stageByName: map[string]groupedStatusLaunchingStageETA{
				"deps_installing": {
					p50:     2 * time.Minute,
					samples: 6,
					oldest:  now.Add(-15 * 24 * time.Hour).Unix(),
				},
			},
			stageEnteredAtByLaunch: map[int64]int64{launchID: now.Add(-30 * time.Second).Unix()},
		},
	})
	want := "~1m remaining"
	if !strings.Contains(out, want) {
		t.Fatalf("missing stage ETA %q in output:\n%s", want, out)
	}
	if strings.Contains(out, "overdue") {
		t.Fatalf("used total-bootstrap ETA instead of mature stage ETA:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_PausedJobBucketsToPausedSection(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{ID: 81, Status: db.StatusPaused, Host: "cool30", Project: "proj", Description: "paused job"},
	}
	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	if !strings.Contains(out, "Paused (1):") {
		t.Fatalf("expected Paused section, got:\n%s", out)
	}
	if strings.Contains(out, "Running (") {
		t.Fatalf("did not expect Running section for paused job, got:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_OpenIntentBucketsToPlacing(t *testing.T) {
	// A job with an open MoveIntent / PlacementIntent is mid-placement and
	// should not flicker through Unplaced or Launching while the move runs.
	now := time.Unix(5_000, 0)
	srcLaunch := int64(7)
	jobs := []*db.Job{
		// Rental-source job, queued, open intent — bulk move-to-new in flight.
		{ID: 1657, Status: db.StatusQueued, LaunchID: &srcLaunch, Project: "proj", Description: "rental move"},
		// Inventory-source job, pending_placement, open intent — autopilot
		// relaunch in flight.
		{ID: 1664, Status: db.StatusPendingPlacement, Project: "proj", Description: "relaunch"},
		// Control: queued job with no intent — must NOT render as Placing.
		{ID: 1700, Status: db.StatusQueued, Host: "cool30", Project: "proj", Description: "ordinary queued"},
	}
	placing := map[int64]struct{}{1657: {}, 1664: {}}
	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, placing, nil, nil, now)
	if !strings.Contains(out, "Placing (2):") {
		t.Fatalf("expected Placing section with 2 jobs, got:\n%s", out)
	}
	if strings.Contains(out, "Unplaced (") {
		t.Fatalf("did not expect Unplaced section for jobs with open intents, got:\n%s", out)
	}
	if strings.Contains(out, "Launching (") {
		t.Fatalf("did not expect Launching section for jobs with open intents, got:\n%s", out)
	}
	if !strings.Contains(out, "Queued (1):") {
		t.Fatalf("expected ordinary queued job to remain in Queued section, got:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_OpenIntentDoesNotOverrideRunning(t *testing.T) {
	// A stale intent on a job that has already started running must not
	// drag it back to the Placing bucket.
	now := time.Unix(5_000, 0)
	launchID := int64(9)
	jobs := []*db.Job{
		{ID: 42, Status: db.StatusRunning, LaunchID: &launchID, Project: "proj", Command: "python a.py", StartTime: 4_400},
	}
	placing := map[int64]struct{}{42: {}}
	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, placing, nil, nil, now)
	if !strings.Contains(out, "Running (1):") {
		t.Fatalf("expected Running section, got:\n%s", out)
	}
	if strings.Contains(out, "Placing (") {
		t.Fatalf("did not expect Placing section for running job, got:\n%s", out)
	}
}

func TestAppendRecentFailedLaunchRows_NilOrEmptyEmitsNothing(t *testing.T) {
	now := time.Unix(10_000, 0)
	if got := appendRecentFailedLaunchRows(nil, nil, 0, 0, now); got != nil {
		t.Fatalf("nil failures should produce no rows, got %d", len(got))
	}
	empty := &recentLaunchFailures{items: nil, windowSince: time.Unix(9_000, 0)}
	if got := appendRecentFailedLaunchRows(nil, empty, 0, 0, now); got != nil {
		t.Fatalf("empty items should produce no rows, got %d", len(got))
	}
}

func TestRenderJobListGroupedStatusPlainAt_RecentLaunchFailuresSection(t *testing.T) {
	now := time.Unix(10_000, 0)
	failures := &recentLaunchFailures{
		items: []*db.Launch{
			{
				ID:                100,
				Status:            db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonProviderFailure,
				TerminationDetail: "container exit 1",
				EndedAt:           testInt64Ptr(9_400),
				ResolvedGPUName:   "RTX 4090",
				CostPerHourCents:  42,
			},
			{
				ID:                101,
				Status:            db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonProviderFailure,
				TerminationDetail: "ssh handshake refused",
				EndedAt:           testInt64Ptr(9_500),
			},
			{
				ID:                102,
				Status:            db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonBootstrapTimeout,
				TerminationDetail: "agent never reached ready",
				EndedAt:           testInt64Ptr(9_600),
			},
		},
		recoveredIDs: map[int64]bool{
			101: true,
		},
		projectByLaunchID: map[int64]string{
			100: "augur",
			101: "augur",
			102: "augur",
		},
		windowSince: now.Add(-30 * time.Minute),
	}

	out := renderJobListGroupedStatusPlainAt(nil, 0, nil, nil, nil, nil, failures, now)

	for _, want := range []string{
		"Recent launch failures (3 in last 30m, 1 already replaced):",
		"reason: provider-side failure (2)",
		"reason: bootstrap timeout (1)",
		"container exit 1",
		"agent never reached ready",
	} {
		if !strings.Contains(stripANSI(out), want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}

	// Recovered failure (id 101) renders dim — verify by checking the
	// styled output retains an ANSI faint sequence for that line.
	if !strings.Contains(out, "ssh handshake refused") {
		t.Fatalf("missing recovered failure detail in output:\n%s", out)
	}
}

func TestAppendRecentFailedLaunchRows_OverflowAddsMoreLine(t *testing.T) {
	now := time.Unix(10_000, 0)
	items := make([]*db.Launch, 0, 10)
	for i := 0; i < 10; i++ {
		items = append(items, &db.Launch{
			ID:                int64(200 + i),
			Status:            db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonProviderFailure,
			TerminationDetail: "boom",
			EndedAt:           testInt64Ptr(9_500 + int64(i)),
		})
	}
	failures := &recentLaunchFailures{
		items:       items,
		windowSince: now.Add(-30 * time.Minute),
	}

	out := renderJobListGroupedStatusPlainAt(nil, 0, nil, nil, nil, nil, failures, now)
	stripped := stripANSI(out)
	if !strings.Contains(stripped, "+ 4 more (weft instance list --status failed)") {
		t.Fatalf("missing overflow line in output:\n%s", out)
	}
	if !strings.Contains(stripped, "Recent launch failures (10 in last 30m):") {
		t.Fatalf("expected total of 10 in header, got:\n%s", out)
	}
}
