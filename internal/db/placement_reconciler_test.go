package db

import (
	"testing"
	"time"
)

// Intent-reconciliation tests (open MoveIntent / PlacementIntent rows being
// converged by a periodic Go-side pass) were removed alongside the periodic
// reconciler in commit XX. The same convergence is now performed atomically
// by DB triggers in 00004_intent_auto_resolution.sql +
// 00005_intent_auto_resolution_launch_status.sql; their behavior is
// exercised by TestTrigger_AutoConfirmMoveIntentOnAttemptInsert (and
// siblings) in intent_triggers_test.go.

func TestReconcileStaleCloudHostJobs_ReturnsToUnplaced(t *testing.T) {
	database := setupTestDB(t)
	now := time.Unix(50_000, 0)

	terminal, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch terminal: %v", err)
	}
	insertTestJob(t, database, 4106, "python train.py", "/tmp", StatusQueued)
	if _, err := database.Exec(
		`UPDATE job_attempts SET host = ?, launch_id = NULL WHERE job_id = ? AND end_time IS NULL`,
		LaunchHost(terminal), 4106,
	); err != nil {
		t.Fatalf("set stale host: %v", err)
	}

	result, err := ReconcileStaleCloudHostJobs(database, ReconcileStaleCloudHostJobsOptions{Now: now})
	if err != nil {
		t.Fatalf("ReconcileStaleCloudHostJobs: %v", err)
	}
	if result.JobsUpdated != 1 {
		t.Fatalf("JobsUpdated = %d, want 1", result.JobsUpdated)
	}
	job, err := GetJobByID(database, 4106)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Host != "" || job.LaunchID != nil {
		t.Fatalf("job placement = host %q launch %v, want unplaced", job.Host, job.LaunchID)
	}
}

func TestRefreshStalePlacementReasons(t *testing.T) {
	database := setupTestDB(t)

	// Unplaced queued job carrying only a stale rebalance reason (a failed/
	// reverted instance→instance move) — must be superseded.
	staleID, err := RecordQueued(database, "", "/tmp/project", "python a.py", "stale")
	if err != nil {
		t.Fatalf("record stale: %v", err)
	}
	if err := SetJobPlacementReasons(database, staleID, []string{"rebalanced from wi1 → wi2 (cost ratio 1.00)"}); err != nil {
		t.Fatalf("set stale reasons: %v", err)
	}

	// Unplaced queued job with a live block reason plus a stale rebalance
	// reason — the rebalance reason drops, the live reason survives.
	mixedID, err := RecordQueued(database, "", "/tmp/project", "python b.py", "mixed")
	if err != nil {
		t.Fatalf("record mixed: %v", err)
	}
	liveReason := "waiting for producer wj999"
	if err := SetJobPlacementReasons(database, mixedID, []string{liveReason, "rebalanced from wi3 → wi4 (cost ratio 0.90)"}); err != nil {
		t.Fatalf("set mixed reasons: %v", err)
	}

	// Unplaced queued job with only a live block reason — untouched.
	liveID, err := RecordQueued(database, "", "/tmp/project", "python c.py", "live")
	if err != nil {
		t.Fatalf("record live: %v", err)
	}
	if err := SetJobPlacementReasons(database, liveID, []string{"no offers available"}); err != nil {
		t.Fatalf("set live reasons: %v", err)
	}

	res, err := RefreshStalePlacementReasons(database)
	if err != nil {
		t.Fatalf("RefreshStalePlacementReasons: %v", err)
	}
	if res.JobsUpdated != 2 {
		t.Fatalf("JobsUpdated = %d, want 2", res.JobsUpdated)
	}

	reasonsOf := func(id int64) []string {
		job, err := GetJobByID(database, id)
		if err != nil {
			t.Fatalf("get job %d: %v", id, err)
		}
		return job.PlacementReasons
	}

	if got := reasonsOf(staleID); len(got) != 1 || got[0] != queuedAwaitingPlacementReason {
		t.Errorf("stale job reasons = %v, want [%q]", got, queuedAwaitingPlacementReason)
	}
	if got := reasonsOf(mixedID); len(got) != 1 || got[0] != liveReason {
		t.Errorf("mixed job reasons = %v, want [%q]", got, liveReason)
	}
	if got := reasonsOf(liveID); len(got) != 1 || got[0] != "no offers available" {
		t.Errorf("live job reasons = %v, want unchanged", got)
	}
}
