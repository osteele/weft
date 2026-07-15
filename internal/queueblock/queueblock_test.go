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

func TestDisplay_OfferFetchUnavailableIsWaiting(t *testing.T) {
	job := &db.Job{ID: 1, Status: db.StatusQueued, QueueBlockedReason: "planner: offer fetch unavailable"}
	got := Display(job, nil)
	if got.Blocked {
		t.Fatalf("expected Blocked=false for retryable offer fetch, got %#v", got)
	}
	if got.Status != "waiting" || got.Kind != "waiting" {
		t.Fatalf("Display = %#v, want waiting status/kind", got)
	}
	for _, want := range []string{"planner: provider offer fetch unavailable", "market unknown", "Weft will retry"} {
		if !strings.Contains(got.Reason, want) {
			t.Fatalf("Reason = %q, want %q", got.Reason, want)
		}
	}
}

func TestDisplay_RunawayPauseIsPaused(t *testing.T) {
	job := &db.Job{ID: 1, Status: db.StatusQueued, QueueBlockedReason: "new-instance retry paused: repeated infrastructure failures without progress"}
	got := Display(job, nil)
	if got.Blocked {
		t.Fatalf("expected Blocked=false for paused retry breaker, got %#v", got)
	}
	if got.Status != "paused" || got.Kind != "paused" {
		t.Fatalf("Display = %#v, want paused status/kind", got)
	}
	if got.Reason != "repeated infrastructure failures without progress" {
		t.Fatalf("Reason = %q, want display reason without paused prefix", got.Reason)
	}
}

func TestReasonKindAgreesWithBlockreason(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"planner: offer fetch unavailable", KindWaiting},
		{"inventory-tagged: waiting for on-prem host", KindWaiting},
		{"offer unavailable: pod create --gpu-id: requested instance type is no longer available; Weft will retry with fresh offers", KindWaiting},
		{"provider rejected request (vastai create-instance) (contract 43292019); Weft will retry with fresh offers", KindWaiting},
		{`waiting for "output/model.pt" from wj1570 (queued)`, KindWaiting},
		{`waiting for "output/model.pt" from wj1570 (running)`, KindWaiting},
		{`waiting for "output/model.pt" from wj1570 (failed)`, KindBlocked},
		{"planner: search offers: provider rejected request: 400 invalid filter", KindBlocked},
		{"new-instance retry blocked: paused: repeated launch failures without progress", KindPaused},
		{"gpu gate: no GPU with 26GB free", KindBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			got := ReasonKind(tc.reason)
			if got != tc.want {
				t.Fatalf("ReasonKind = %q, want %q", got, tc.want)
			}
		})
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
