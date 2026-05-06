package db

import (
	"database/sql"
	"testing"
	"time"
)

// TestRepairCloudCompletions_RearmsPoisonedRows verifies that
// FindPoisonedCloudCompletions selects the production-observed shape
// (start_time=0, populated end_time, last_synced_status matching status)
// and RearmPoisonedCloudCompletions clears last_synced_status so a later
// sync re-runs backfill.
func TestRepairCloudCompletions_RearmsPoisonedRows(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	syncTime := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, start_time = 0, end_time = ?, exit_code = 0, last_synced_status = ?
		 WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		StatusCompleted, syncTime, StatusCompleted, jobID,
	); err != nil {
		t.Fatalf("seed poisoned row: %v", err)
	}

	// Updated NeedsCloudCompletionBackfill arms backfill regardless of
	// last_synced_status when start_time=0 — safety net for future poisoning.
	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if !needs {
		t.Fatal("needs backfill = false, want true (start_time=0 should arm backfill)")
	}

	found, err := FindPoisonedCloudCompletions(database)
	if err != nil {
		t.Fatalf("FindPoisonedCloudCompletions: %v", err)
	}
	if len(found) != 1 || found[0].JobID != jobID {
		t.Fatalf("FindPoisonedCloudCompletions = %+v, want one row for job %d", found, jobID)
	}

	n, err := RearmPoisonedCloudCompletions(database)
	if err != nil {
		t.Fatalf("RearmPoisonedCloudCompletions: %v", err)
	}
	if n != 1 {
		t.Fatalf("RearmPoisonedCloudCompletions affected %d rows, want 1", n)
	}

	var lastSynced sql.NullString
	if err := database.QueryRow(
		`SELECT last_synced_status FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&lastSynced); err != nil {
		t.Fatalf("query last_synced_status: %v", err)
	}
	if lastSynced.Valid {
		t.Fatalf("last_synced_status = %q, want NULL after repair", lastSynced.String)
	}

	// Simulate a follow-up sync rewriting authoritative start_time. After
	// that, FindPoisonedCloudCompletions must not match the row.
	if _, err := database.Exec(
		`UPDATE job_attempts SET start_time = ? WHERE id = ?`,
		int64(syncTime-3600), found[0].AttemptID,
	); err != nil {
		t.Fatalf("simulate backfill: %v", err)
	}
	again, err := FindPoisonedCloudCompletions(database)
	if err != nil {
		t.Fatalf("FindPoisonedCloudCompletions (post-backfill): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("post-backfill FindPoisonedCloudCompletions = %+v, want empty", again)
	}
}
