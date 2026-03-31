package campaign

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

func TestFinalizeStuckJobsWithR2Check_RecoverFromR2(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a completed launch (instance already finished).
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	// Create a job with a non-terminal attempt on the completed launch.
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, 1); err != nil {
		t.Fatalf("create job: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ? WHERE job_id = 1 AND end_time IS NULL`,
		db.StatusRunning, instanceID)

	// Mock CheckAndSyncJobComplete to simulate R2 having the .complete marker.
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, dbConn *sql.DB, jobID int64) bool {
		// Simulate recording the completion from R2.
		dbConn.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
			db.StatusCompleted, time.Now().Unix(), jobID)
		return true
	}

	repaired, err := FinalizeStuckJobsWithR2Check(database, &r2.Client{})
	if err != nil {
		t.Fatalf("FinalizeStuckJobsWithR2Check: %v", err)
	}
	if len(repaired) != 1 || repaired[0] != 1 {
		t.Errorf("repaired = %v, want [1]", repaired)
	}

	// The job should be completed (recovered from R2), NOT dead.
	var status string
	database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 1 ORDER BY id DESC LIMIT 1`).Scan(&status)
	if status != string(db.StatusCompleted) {
		t.Errorf("job status = %q, want %q (should be recovered from R2, not marked dead)", status, db.StatusCompleted)
	}
}

func TestFinalizeStuckJobsWithR2Check_FallbackToDead(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a completed launch.
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	// Create a job with a non-terminal attempt on the completed launch.
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, 1); err != nil {
		t.Fatalf("create job: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ? WHERE job_id = 1 AND end_time IS NULL`,
		db.StatusRunning, instanceID)

	// Mock CheckAndSyncJobComplete to simulate R2 NOT having the .complete marker.
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, _ *sql.DB, _ int64) bool {
		return false
	}

	repaired, err := FinalizeStuckJobsWithR2Check(database, &r2.Client{})
	if err != nil {
		t.Fatalf("FinalizeStuckJobsWithR2Check: %v", err)
	}
	if len(repaired) != 1 || repaired[0] != 1 {
		t.Errorf("repaired = %v, want [1]", repaired)
	}

	// The job should be dead (no R2 completion data available).
	var status string
	database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 1 ORDER BY id DESC LIMIT 1`).Scan(&status)
	if status != string(db.StatusDead) {
		t.Errorf("job status = %q, want %q (should be marked dead when R2 has no data)", status, db.StatusDead)
	}
}

func TestFinalizeStuckJobsWithR2Check_NilR2Client(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a completed launch.
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	// Create a job with a non-terminal attempt on the completed launch.
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, 1); err != nil {
		t.Fatalf("create job: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ? WHERE job_id = 1 AND end_time IS NULL`,
		db.StatusRunning, instanceID)

	// With nil R2 client, should fall back to marking dead.
	repaired, err := FinalizeStuckJobsWithR2Check(database, nil)
	if err != nil {
		t.Fatalf("FinalizeStuckJobsWithR2Check: %v", err)
	}
	if len(repaired) != 1 {
		t.Errorf("repaired = %v, want [1]", repaired)
	}

	var status string
	database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 1 ORDER BY id DESC LIMIT 1`).Scan(&status)
	if status != string(db.StatusDead) {
		t.Errorf("job status = %q, want %q", status, db.StatusDead)
	}
}
