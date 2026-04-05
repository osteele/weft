package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestRenderJobListGroupedStatusPlainSectionsAndOrder(t *testing.T) {
	launchID := int64(44)
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "cool30", Project: "proj", Description: "run a"},
		{ID: 2, Status: db.StatusStarting, Host: "cool30", Project: "proj", Description: "run b"},
		{ID: 3, Status: db.StatusQueued, Host: "cool30", Project: "proj", Description: "queue"},
		{ID: 10, Status: db.StatusPendingPlacement, Host: "", Project: "proj", Description: "needs placement"},
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
		"Unplaced (1):",
		"Completions (1):",
		"Failures (3):",
		"Killed/Canceled (2):",
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

	if !strings.Contains(out, "- 4 — proj ok (inventory) — completed ok") {
		t.Fatalf("missing completion line in output:\n%s", out)
	}
	if !strings.Contains(out, "- 7 — proj bad exit (inventory) — completed (exit 2)") {
		t.Fatalf("missing failed-completed line in output:\n%s", out)
	}
	if !strings.Contains(out, "- 9 — proj canceled (rental) — canceled") {
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
			ExitCode:    testIntPtr(0),
		},
	}

	out := renderJobListGroupedStatusPlainAt(jobs, 0, map[int64]*db.LaunchLiveState{
		launchID: {LaunchID: launchID, JobProgressID: 42, JobProgressPct: 75},
	}, now)

	for _, want := range []string{
		"- 42 — proj python train.py (rental) — running 75% — running 10m ago",
		"- 43 — proj queued — queued 13m ago",
		"- 44 — proj done — placed 30m ago — completed ok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
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

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, now)
	want := "- 50 — proj waiting — queued 5m ago — blocked: first retry budget exceeded: elapsed 1h2m >= limit 45m"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
}

func TestRenderJobListGroupedStatusPlainAt_ShowsRetriedTimingForUnplacedRetry(t *testing.T) {
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

	out := renderJobListGroupedStatusPlainAt(jobs, 0, nil, now)
	want := "- 51 — proj retry me — retried 1m ago"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in output:\n%s", want, out)
	}
}
