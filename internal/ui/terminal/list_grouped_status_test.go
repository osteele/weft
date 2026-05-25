package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/jobview"
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

func TestGroupedStatusPlacementMarkerInterruptibleRental(t *testing.T) {
	glyph, jobID := groupedStatusPlacementMarker(&db.Job{
		ID:     123,
		Host:   db.LaunchHost(44),
		Tags:   []string{db.TagInterruptible},
		Status: db.StatusRunning,
	})

	if got := stripANSI(glyph + " " + jobID); got != "☁ wj123" {
		t.Fatalf("marker = %q, want cloud interruptible marker", got)
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

func TestRenderJobListGroupedStatusPlainAt_FailuresUseEndTime(t *testing.T) {
	now := time.Unix(5_000, 0)
	end := int64(4_940)
	jobs := []*db.Job{
		{
			ID:          54,
			Status:      db.StatusFailed,
			Project:     "proj",
			Description: "failed run",
			QueuedAt:    4_000,
			EndTime:     &end,
		},
		{
			ID:          55,
			Status:      db.StatusCompleted,
			Project:     "proj",
			Description: "bad exit",
			QueuedAt:    4_000,
			EndTime:     &end,
			ExitCode:    testIntPtr(2),
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	for _, want := range []string{
		"-   wj54 — proj failed run — failed 1m ago — failed",
		"-   wj55 — proj bad exit — failed 1m ago — completed (exit 2)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestRenderJobListGroupedStatusPlainAt_FailuresFallbackToPlacedWhenEndMissing(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:          56,
			Status:      db.StatusFailed,
			Project:     "proj",
			Description: "legacy failure",
			QueuedAt:    4_700,
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	want := "-   wj56 — proj legacy failure — placed 5m ago — failed"
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

func TestRenderJobListGroupedStatusPlainAt_UsesPersistedPlacementReason(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:          1986,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "needs placement",
			CreatedAt:   4_400,
			PlacementReasons: []string{
				"planner: older reason",
				"planner: no offers from providers for gpu=A100 vram>=82GB disk>=50GB reliability>=0.85",
			},
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	blockedWant := "  blocked: planner: no offers from providers for gpu=A100 vram>=82GB disk>=50GB reliability>=0.85 (1)"
	if !strings.Contains(out, blockedWant) {
		t.Fatalf("missing %q in output:\n%s", blockedWant, out)
	}
	if strings.Contains(out, "older reason") {
		t.Fatalf("used stale placement reason in output:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_HidesOnPremOnlyPlacementReasonForRentalEligibleJob(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:          1997,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "needs placement",
			CreatedAt:   4_400,
			PlacementReasons: []string{
				"no local host matched gpu-class=nvidia, gpu-mem>=8GB",
				"3 hosts: host is opt-in only (specify with --host)",
			},
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	if strings.Contains(out, "blocked:") {
		t.Fatalf("unexpected blocked reason for rental-eligible job:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_DoesNotFallBackToOlderPersistedPlacementReason(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:          1997,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "needs placement",
			CreatedAt:   4_400,
			PlacementReasons: []string{
				"planner: no offers from providers for gpu=A100 vram>=82GB disk>=50GB",
				"3 hosts: host is opt-in only (specify with --host)",
			},
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	if strings.Contains(out, "blocked:") {
		t.Fatalf("unexpected fallback to stale blocked reason:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_HidesStaleReuseFailureForPlacedQueuedJob(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(3022)
	jobs := []*db.Job{
		{
			ID:          2031,
			Status:      db.StatusQueued,
			LaunchID:    &launchID,
			Project:     "contour-pareto",
			Description: "EXP-110 HMC-marginalized hyperparameter search",
			QueuedAt:    4_900,
			PlacementReasons: []string{
				"reuse instance wi3022 failed: claim job wj2031 for instance wi3022: set launch_id for job wj2031: job 2031: job already claimed by another launch",
			},
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	if strings.Contains(out, "blocked:") {
		t.Fatalf("unexpected stale reuse blocker for placed queued job:\n%s", out)
	}
	if !strings.Contains(out, "Queued (1):") {
		t.Fatalf("missing queued section:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_HidesUnplacedResetPlacementReason(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:          2006,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "needs relaunch",
			CreatedAt:   4_400,
			PlacementReasons: []string{
				"cloud instance 2968 unavailable; job reset to unplaced queue",
			},
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	if strings.Contains(out, "blocked:") {
		t.Fatalf("unexpected blocked reason for unplaced reset:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_HidesFailedCloudResetPlacementReason(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:          2025,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "needs relaunch",
			CreatedAt:   4_400,
			PlacementReasons: []string{
				"cloud instance 3025 failed (infra_failure)",
			},
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	if strings.Contains(out, "blocked:") {
		t.Fatalf("unexpected blocked reason for failed cloud reset:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_UsesActionableReasonAfterFailedCloudReset(t *testing.T) {
	now := time.Unix(5_000, 0)
	actionable := "planner: no offers from providers for gpu=A100 vram>=82GB"
	jobs := []*db.Job{
		{
			ID:          2028,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "needs placement",
			CreatedAt:   4_400,
			PlacementReasons: []string{
				"cloud instance 3025 failed (infra_failure)",
				actionable,
			},
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	blockedWant := "  blocked: " + actionable + " (1)"
	if !strings.Contains(out, blockedWant) {
		t.Fatalf("missing %q in output:\n%s", blockedWant, out)
	}
	if strings.Contains(out, "cloud instance 3025 failed") {
		t.Fatalf("unexpected failed cloud reset reason:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_ShowsOnPremOnlyPlacementReasonForInventoryJob(t *testing.T) {
	now := time.Unix(5_000, 0)
	jobs := []*db.Job{
		{
			ID:          1997,
			Status:      db.StatusQueued,
			Project:     "proj",
			Description: "needs placement",
			CreatedAt:   4_400,
			Tags:        []string{db.TagInventory},
			PlacementReasons: []string{
				"no local host matched gpu-class=nvidia, gpu-mem>=8GB",
				"3 hosts: host is opt-in only (specify with --host)",
			},
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, nil, nil, nil, now)
	blockedWant := "  blocked: 3 hosts: host is opt-in only (specify with --host) (1)"
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

func groupedRowsText(rows []groupedStatusRow) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.text)
		b.WriteString("\n")
	}
	return b.String()
}

func TestBuildGroupedStatusRows_HoistsLaunchAndExpandsDisclosure(t *testing.T) {
	now := time.Unix(5_000, 0)
	mkJob := func(id int64, desc string) *db.Job {
		return &db.Job{
			ID:                 id,
			Status:             db.StatusQueued,
			Project:            "proj",
			Description:        desc,
			QueuedAt:           4_000,
			QueueBlockedReason: "no rental headroom; running instances couldn't accept this job",
		}
	}
	jobs := []*db.Job{
		mkJob(2030, "continuous-thought EXP-051"),
		mkJob(2029, "adaptive-escalation EXP-011"),
	}
	detail := map[int64]*blockreason.Structured{
		2030: {
			Summary: "no rental headroom; running instances couldn't accept this job: disk insufficient",
			Launch:  "no rental headroom",
			Reuse: []blockreason.ReuseRejection{
				{Instance: "wi1023", Reason: "disk insufficient: need=42GB free=12GB"},
				{Instance: "wi1044", Reason: "GPU class mismatch: job=ampere instance=ada"},
			},
		},
		2029: {
			Summary: "no rental headroom; running instances couldn't accept this job",
			Launch:  "no rental headroom",
			Reuse: []blockreason.ReuseRejection{
				{Instance: "wi1023", Reason: "grace period too short: 40s remaining"},
			},
		},
	}
	opts := groupedStatusRenderOptions{
		now:             now,
		blockedDetail:   detail,
		expandedBlocked: map[int64]bool{2030: true},
	}
	out := groupedRowsText(buildGroupedStatusRowsWithOptions(jobs, 0, opts))

	if c := strings.Count(out, "launch blocked for all: no rental headroom"); c != 1 {
		t.Fatalf("expected launch blocker hoisted to exactly one line, got %d:\n%s", c, out)
	}
	if !strings.Contains(out, "▾") || !strings.Contains(out, "▸") {
		t.Fatalf("expected both expanded (▾) and collapsed (▸) disclosure markers:\n%s", out)
	}
	for _, want := range []string{
		"reuse wi1023  disk insufficient: need=42GB free=12GB",
		"reuse wi1044  GPU class mismatch: job=ampere instance=ada",
		"new instance  no rental headroom",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing expanded detail line %q:\n%s", want, out)
		}
	}
	// The collapsed job's per-instance disclosure detail must not be rendered.
	// Its reuse headline still appears on the bucket subheader; only the
	// indented "reuse <instance>  ..." detail line should be absent.
	if strings.Contains(out, "reuse wi1023  grace period") {
		t.Fatalf("collapsed job disclosure detail leaked into output:\n%s", out)
	}
}

func TestBuildGroupedStatusRows_NoHoistWhenLaunchBlockersDiffer(t *testing.T) {
	now := time.Unix(5_000, 0)
	mkJob := func(id int64) *db.Job {
		return &db.Job{
			ID:                 id,
			Status:             db.StatusQueued,
			Project:            "proj",
			Description:        "desc",
			QueuedAt:           4_000,
			QueueBlockedReason: "blocked",
		}
	}
	jobs := []*db.Job{mkJob(1), mkJob(2)}
	detail := map[int64]*blockreason.Structured{
		1: {Summary: "a", Launch: "no rental headroom", Reuse: []blockreason.ReuseRejection{{Instance: "wi1", Reason: "disk insufficient"}}},
		2: {Summary: "b", Launch: "no compatible offers", Reuse: []blockreason.ReuseRejection{{Instance: "wi1", Reason: "disk insufficient"}}},
	}
	out := groupedRowsText(buildGroupedStatusRowsWithOptions(jobs, 0, groupedStatusRenderOptions{
		now:           now,
		blockedDetail: detail,
	}))
	if strings.Contains(out, "launch blocked for all:") {
		t.Fatalf("launch blocker should not be hoisted when blockers differ:\n%s", out)
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
	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, nil, placing, map[int64]int64{1657: 4_000}, nil, now)
	if !strings.Contains(out, "Placing (2):") {
		t.Fatalf("expected Placing section with 2 jobs, got:\n%s", out)
	}
	if !strings.Contains(out, "wj1657") || !strings.Contains(out, "— placing 16m ago") {
		t.Fatalf("expected Placing row to use open intent age, got:\n%s", out)
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

func TestRenderJobListGroupedStatusPlainWithOptions_UsesPlacementStatusReadModel(t *testing.T) {
	now := time.Unix(5_000, 0)
	launchID := int64(12)
	jobs := []*db.Job{
		{ID: 1801, Status: db.StatusQueued, LaunchID: &launchID, Project: "proj", Description: "moving"},
	}
	out := renderJobListGroupedStatusPlainWithOptions(jobs, 0, groupedStatusRenderOptions{
		placementStatusByJob: map[int64]jobview.PlacementStatus{
			1801: {JobID: 1801, Bucket: jobview.BucketPlacing, DisplayAt: 4_000, HasOpenIntent: true, LaunchID: &launchID},
		},
		placementQueuedAtByJob: map[int64]int64{1801: 4_000},
		now:                    now,
	})
	if !strings.Contains(out, "Placing (1):") {
		t.Fatalf("expected Placing section from placement read model, got:\n%s", out)
	}
	if !strings.Contains(out, "— placing 16m ago") {
		t.Fatalf("expected placement read-model time, got:\n%s", out)
	}
}

func failedInstanceRowText(rows []groupedStatusRow) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, stripANSI(r.text))
	}
	return strings.Join(parts, "\n")
}

func TestAppendRecentFailedInstanceRows_NilOrEmptyEmitsNothing(t *testing.T) {
	now := time.Unix(10_000, 0)
	if got := appendRecentFailedInstanceRows(nil, nil, 0, 0, false, false, now); got != nil {
		t.Fatalf("nil failures should produce no rows, got %d", len(got))
	}
	empty := &recentFailedInstances{items: nil}
	if got := appendRecentFailedInstanceRows(nil, empty, 0, 0, false, false, now); got != nil {
		t.Fatalf("empty items should produce no rows, got %d", len(got))
	}
}

func TestAppendRecentFailedInstanceRows_FiltersNormalTerminationsAndClassifies(t *testing.T) {
	now := time.Unix(10_000, 0)
	failures := &recentFailedInstances{
		items: []*db.Launch{
			nil,
			{
				ID:                100,
				Status:            db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonUnknown,
				TerminationDetail: "provider dead",
				EndedAt:           testInt64Ptr(9_500),
			},
			{
				ID:                101,
				Status:            db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonUnknown,
				TerminationDetail: "provider dead",
				EndedAt:           testInt64Ptr(9_600),
			},
			{
				ID:                102,
				Status:            db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonJobFailure,
				TerminationDetail: "job failed",
				EndedAt:           testInt64Ptr(9_700),
			},
		},
		// 100 has no job outcome — a dud. 101's job completed — a succeeded
		// series. 102 is a normal job failure and is filtered out.
		jobOutcomeByLaunchID: map[int64]db.LaunchJobOutcome{
			101: {JobID: 1, Status: db.StatusCompleted},
		},
	}

	out := stripANSI(renderJobListGroupedStatusPlainAt(nil, 0, nil, nil, nil, nil, failures, now))
	for _, want := range []string{
		"Recent failed instances — last 8m (2):",
		"1 succeeded · 1 dud — last 8m",
		"succeeded (1) — 6m ago:",
		"dud (1) — 8m ago:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(0)") {
		t.Fatalf("header used a stale zero count:\n%s", out)
	}
	if strings.Contains(out, "job failed") {
		t.Fatalf("normal job failure should be hidden:\n%s", out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_RecentFailedInstancesSection(t *testing.T) {
	now := time.Unix(10_000, 0)
	failures := &recentFailedInstances{
		items: []*db.Launch{
			{
				ID:                100,
				Status:            db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonProviderFailure,
				TerminationDetail: "container exit 1",
				EndedAt:           testInt64Ptr(9_400),
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
		projectByLaunchID: map[int64]string{100: "augur", 101: "augur", 102: "augur"},
		jobOutcomeByLaunchID: map[int64]db.LaunchJobOutcome{
			100: {JobID: 1, Status: db.StatusFailed},
			101: {JobID: 2, Status: db.StatusRunning},
			102: {JobID: 3, Status: db.StatusQueued},
		},
	}

	out := stripANSI(renderJobListGroupedStatusPlainAt(nil, 0, nil, nil, nil, nil, failures, now))

	for _, want := range []string{
		"Recent failed instances — last 10m (3):",
		"1 failed · 1 awaiting placement · 1 ongoing — last 10m",
		"failed (1) — 10m ago:",
		"awaiting placement (1) — 6m ago:",
		"ongoing (1) — 8m ago:",
		"container exit 1",
		"agent never reached ready",
		// An ongoing series annotates the row so the user knows work continues.
		"ssh handshake refused, relaunching",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestAppendRecentFailedInstanceRows_ExpandTogglesFYIBuckets(t *testing.T) {
	now := time.Unix(10_000, 0)
	failures := &recentFailedInstances{
		items: []*db.Launch{
			{ID: 400, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonProviderFailure, TerminationDetail: "boom", EndedAt: testInt64Ptr(9_700)},
			{ID: 401, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonProviderFailure, TerminationDetail: "transient", EndedAt: testInt64Ptr(9_600)},
			{ID: 402, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonProviderFailure, TerminationDetail: "transient", EndedAt: testInt64Ptr(9_500)},
			{ID: 403, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonInfraFailure, TerminationDetail: "no agent", EndedAt: testInt64Ptr(9_400)},
		},
		jobOutcomeByLaunchID: map[int64]db.LaunchJobOutcome{
			400: {JobID: 1, Status: db.StatusFailed},
			401: {JobID: 2, Status: db.StatusCompleted},
			402: {JobID: 3, Status: db.StatusCompleted},
			// 403 has no job outcome — a dud.
		},
	}

	collapsed := failedInstanceRowText(appendRecentFailedInstanceRows(nil, failures, 0, 0, true, false, now))
	if !strings.Contains(collapsed, "failed (1)") {
		t.Fatalf("collapsed view should always show the failed bucket:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "+ 3 more: 2 succeeded, 1 dud") {
		t.Fatalf("collapsed view should roll up the FYI buckets:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "▸") {
		t.Fatalf("collapsed toggle should carry a ▸ marker:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "succeeded (2)") || strings.Contains(collapsed, "dud (1)") {
		t.Fatalf("collapsed view should hide the FYI bucket sub-headers:\n%s", collapsed)
	}

	expanded := failedInstanceRowText(appendRecentFailedInstanceRows(nil, failures, 0, 0, true, true, now))
	if !strings.Contains(expanded, "▾") {
		t.Fatalf("expanded toggle should carry a ▾ marker:\n%s", expanded)
	}
	for _, want := range []string{"succeeded (2)", "dud (1)"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded view should reveal %q:\n%s", want, expanded)
		}
	}
}

func TestAppendRecentFailedInstanceRows_ChainTerminalOverridesJobStatus(t *testing.T) {
	now := time.Unix(10_000, 0)
	failures := &recentFailedInstances{
		items: []*db.Launch{
			{ID: 500, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonProviderFailure, TerminationDetail: "x", EndedAt: testInt64Ptr(9_600)},
			{ID: 501, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonProviderFailure, TerminationDetail: "y", EndedAt: testInt64Ptr(9_500)},
		},
		// Both jobs are currently queued, but the relaunch chain's terminal
		// launch is authoritative: a live successor ⇒ ongoing, a completed
		// successor ⇒ succeeded.
		jobOutcomeByLaunchID: map[int64]db.LaunchJobOutcome{
			500: {JobID: 1, Status: db.StatusQueued},
			501: {JobID: 2, Status: db.StatusQueued},
		},
		chainTerminalByLaunchID: map[int64]string{
			500: db.LaunchStatusRunning,
			501: db.LaunchStatusCompleted,
		},
	}

	out := stripANSI(renderJobListGroupedStatusPlainAt(nil, 0, nil, nil, nil, nil, failures, now))
	for _, want := range []string{"ongoing (1)", "succeeded (1)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "awaiting placement") {
		t.Fatalf("chain terminal status should override the queued job status:\n%s", out)
	}
}

func TestAppendRecentFailedInstanceRows_OutcomesClusterAndCost(t *testing.T) {
	now := time.Unix(10_000, 0)
	failures := &recentFailedInstances{
		items: []*db.Launch{
			{
				ID:                300,
				Status:            db.LaunchStatusFailed,
				Provider:          "vastai",
				TerminationReason: db.TerminationReasonInfraFailure,
				TerminationDetail: "provider instance stuck in loading",
				EndedAt:           testInt64Ptr(9_700),
				ActualSpendCents:  11,
			},
			{
				ID:                301,
				Status:            db.LaunchStatusFailed,
				Provider:          "vastai",
				TerminationReason: db.TerminationReasonInfraFailure,
				TerminationDetail: "dud provider: no agent activity",
				EndedAt:           testInt64Ptr(9_600),
				ActualSpendCents:  20,
			},
			{
				ID:                302,
				Status:            db.LaunchStatusFailed,
				Provider:          "vastai",
				TerminationReason: db.TerminationReasonProviderFailure,
				TerminationDetail: "container exit 1",
				EndedAt:           testInt64Ptr(9_500),
			},
		},
		jobOutcomeByLaunchID: map[int64]db.LaunchJobOutcome{
			300: {JobID: 1, Status: db.StatusQueued},
			// 301 has no job outcome — a dud.
			302: {JobID: 2, Status: db.StatusCompleted},
		},
	}

	out := stripANSI(renderJobListGroupedStatusPlainAt(nil, 0, nil, nil, nil, nil, failures, now))
	for _, want := range []string{
		"Recent failed instances — last 8m (3):",
		"1 awaiting placement · 1 succeeded · 1 dud — last 8m · $0.31 wasted",
		"⚠ clustered failures — common factor: provider vastai",
		"awaiting placement (1)",
		"succeeded (1)",
		"dud (1)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

// TestAppendRecentFailedInstanceRows_ProviderTimeoutDoesNotCluster ensures
// that transient provider CLI/API timeouts are excluded from the
// clustered-failures banner. A slow vast.ai API hour produces many
// provider_timeout rows; they must still appear in the recent-failed table
// (so cost/postmortem accounting works), but they must not drive the banner
// to flag "common factor: provider vastai" — that signal is reserved for
// real systemic provider trouble.
func TestAppendRecentFailedInstanceRows_ProviderTimeoutDoesNotCluster(t *testing.T) {
	now := time.Unix(10_000, 0)
	failures := &recentFailedInstances{
		items: []*db.Launch{
			{
				ID:                400,
				Status:            db.LaunchStatusFailed,
				Provider:          "vastai",
				TerminationReason: db.TerminationReasonProviderTimeout,
				TerminationDetail: "instance creation failed: create instance: provider command timed out: vastai create instance 30s",
				EndedAt:           testInt64Ptr(9_700),
			},
			{
				ID:                401,
				Status:            db.LaunchStatusFailed,
				Provider:          "vastai",
				TerminationReason: db.TerminationReasonProviderTimeout,
				TerminationDetail: "instance creation failed: attach SSH key to instance 37326566: attach ssh: provider command timed out",
				EndedAt:           testInt64Ptr(9_650),
			},
			{
				ID:                402,
				Status:            db.LaunchStatusFailed,
				Provider:          "vastai",
				TerminationReason: db.TerminationReasonProviderTimeout,
				TerminationDetail: "instance creation failed: create instance: provider command timed out: vastai create instance 30s",
				EndedAt:           testInt64Ptr(9_600),
			},
		},
		jobOutcomeByLaunchID: map[int64]db.LaunchJobOutcome{
			400: {JobID: 1, Status: db.StatusQueued},
			401: {JobID: 2, Status: db.StatusQueued},
			402: {JobID: 3, Status: db.StatusQueued},
		},
	}

	out := stripANSI(renderJobListGroupedStatusPlainAt(nil, 0, nil, nil, nil, nil, failures, now))

	// Rows must still appear in the table — only the cluster signal is suppressed.
	if !strings.Contains(out, "Recent failed instances") {
		t.Fatalf("expected recent failed instances section, got:\n%s", out)
	}
	if strings.Contains(out, "clustered failures") {
		t.Fatalf("provider_timeout failures must not trigger clustered-failures banner; output:\n%s", out)
	}
}

// TestAppendRecentFailedInstanceRows_RealFailuresStillClusterWhenMixedWithTimeouts
// confirms the filter is narrow: a mix of provider_timeout and real
// infra_failure rows should still surface a cluster on the genuine failures
// alone (when ≥3 of them share a factor). This guards against
// over-aggressive filtering swallowing legitimate clustering signals.
func TestAppendRecentFailedInstanceRows_RealFailuresStillClusterWhenMixedWithTimeouts(t *testing.T) {
	now := time.Unix(10_000, 0)
	failures := &recentFailedInstances{
		items: []*db.Launch{
			// 3 real infra failures on vastai → should cluster.
			{ID: 500, Status: db.LaunchStatusFailed, Provider: "vastai", TerminationReason: db.TerminationReasonInfraFailure, EndedAt: testInt64Ptr(9_700)},
			{ID: 501, Status: db.LaunchStatusFailed, Provider: "vastai", TerminationReason: db.TerminationReasonInfraFailure, EndedAt: testInt64Ptr(9_650)},
			{ID: 502, Status: db.LaunchStatusFailed, Provider: "vastai", TerminationReason: db.TerminationReasonProviderFailure, EndedAt: testInt64Ptr(9_600)},
			// 2 timeouts mixed in — must not dilute the cluster.
			{ID: 503, Status: db.LaunchStatusFailed, Provider: "runpod", TerminationReason: db.TerminationReasonProviderTimeout, EndedAt: testInt64Ptr(9_550)},
			{ID: 504, Status: db.LaunchStatusFailed, Provider: "runpod", TerminationReason: db.TerminationReasonProviderTimeout, EndedAt: testInt64Ptr(9_500)},
		},
		jobOutcomeByLaunchID: map[int64]db.LaunchJobOutcome{
			500: {JobID: 1, Status: db.StatusQueued},
			501: {JobID: 2, Status: db.StatusQueued},
			502: {JobID: 3, Status: db.StatusQueued},
			503: {JobID: 4, Status: db.StatusQueued},
			504: {JobID: 5, Status: db.StatusQueued},
		},
	}

	out := stripANSI(renderJobListGroupedStatusPlainAt(nil, 0, nil, nil, nil, nil, failures, now))
	if !strings.Contains(out, "clustered failures — common factor: provider vastai") {
		t.Fatalf("expected vastai cluster banner from real failures, got:\n%s", out)
	}
}

func TestStripContractRef(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "trailing contract reference removed",
			in:   "machine busy: provider rejected instance creation (contract 37151723)",
			want: "machine busy: provider rejected instance creation",
		},
		{
			name: "inline contract reference removed",
			in:   "rejected (contract 5) by provider",
			want: "rejected by provider",
		},
		{
			name: "reason without a contract reference is unchanged",
			in:   "no offers available",
			want: "no offers available",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockreason.StripContractRef(tc.in); got != tc.want {
				t.Fatalf("StripContractRef(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
