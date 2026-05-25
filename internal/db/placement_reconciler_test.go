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
