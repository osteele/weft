package queueblock

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestDisplay_PendingPlacementWithReasonIsBlocked(t *testing.T) {
	job := &db.Job{ID: 1, Status: db.StatusPendingPlacement, QueueBlockedReason: "waiting for output"}
	got := Display(job, nil)
	if !got.Blocked {
		t.Fatalf("expected Blocked=true, got %#v", got)
	}
	if got.Status != "blocked" {
		t.Errorf("Status = %q, want blocked", got.Status)
	}
	if got.Reason != "waiting for output" {
		t.Errorf("Reason = %q, want waiting for output", got.Reason)
	}
}

func TestDisplay_QueuedWithReasonIsBlocked(t *testing.T) {
	job := &db.Job{ID: 1, Status: db.StatusQueued, QueueBlockedReason: "queue gate failed"}
	got := Display(job, nil)
	if !got.Blocked {
		t.Fatalf("expected Blocked=true, got %#v", got)
	}
	if got.Kind != "blocked" {
		t.Errorf("Kind = %q, want blocked", got.Kind)
	}
}

func TestDisplay_SourceSyncFailureIsWaiting(t *testing.T) {
	job := &db.Job{ID: 1, Status: db.StatusQueued, QueueBlockedReason: "[21s ago, retry #2] source sync failed: rsync timed out"}
	got := Display(job, nil)
	if got.Blocked {
		t.Fatalf("expected Blocked=false for retryable source sync, got %#v", got)
	}
	if got.Status != "waiting" || got.Kind != "waiting" {
		t.Fatalf("Display = %#v, want waiting status/kind", got)
	}
}

func TestDisplay_RunningIsNotBlockable(t *testing.T) {
	job := &db.Job{ID: 1, Status: db.StatusRunning, Host: "cool30", QueueBlockedReason: "ignored"}
	got := Display(job, nil)
	if got.Blocked {
		t.Fatalf("expected Blocked=false for running job, got %#v", got)
	}
}

func TestApply_PendingPlacementClearsAndKeepsHostQueueBlocking(t *testing.T) {
	pending := &db.Job{ID: 1, Status: db.StatusPendingPlacement, QueueBlockedReason: "stale"}
	queuedHost := &db.Job{ID: 2, Status: db.StatusQueued, Host: "cool30", QueueBlockedReason: "stale"}
	running := &db.Job{ID: 3, Status: db.StatusRunning, Host: "cool30", QueueBlockedReason: "stale"}

	lookup := Lookup{"cool30": {2: "gpu busy"}}
	Apply([]*db.Job{pending, queuedHost, running}, lookup)

	if pending.QueueBlockedReason != "" {
		t.Errorf("pending: want cleared (no host-queue lookup hit), got %q", pending.QueueBlockedReason)
	}
	if queuedHost.QueueBlockedReason != "gpu busy" {
		t.Errorf("queuedHost: want %q, got %q", "gpu busy", queuedHost.QueueBlockedReason)
	}
	if running.QueueBlockedReason != "" {
		t.Errorf("running: want cleared, got %q", running.QueueBlockedReason)
	}
}

func TestMissingPayloadReasonDoesNotReportBug(t *testing.T) {
	bugDB := db.SetupTestBugDB(t)

	reason := userVisibleBlockedReason("cool30", 2556, "missing queue payload: open /home/oliver/.cache/weft/queue/job-2556.json: no such file or directory")
	if !strings.Contains(reason, "temporarily inconsistent") {
		t.Fatalf("reason = %q, want temporary inconsistency message", reason)
	}

	bugs, err := db.ListBugs(bugDB, true)
	if err != nil {
		t.Fatalf("ListBugs: %v", err)
	}
	if len(bugs) != 0 {
		t.Fatalf("bug count = %d, want 0; bugs=%v", len(bugs), bugs)
	}
}
