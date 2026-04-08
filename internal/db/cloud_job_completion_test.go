package db

import (
	"testing"
	"time"
)

func TestNeedsCloudCompletionBackfill_TerminalIncomplete(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, start_time = ?, end_time = ?, exit_code = NULL, last_synced_status = ?
		 WHERE job_id = ? AND end_time IS NULL`,
		StatusFailed, int64(100), int64(200), StatusRunning, jobID,
	); err != nil {
		t.Fatalf("seed terminal incomplete attempt: %v", err)
	}

	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if !needs {
		t.Fatal("needs backfill = false, want true")
	}
}

func TestNeedsCloudCompletionBackfill_TerminalComplete(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := RecordCloudJobCompletion(database, jobID, 1, 100, 200, "exit_1"); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}

	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if needs {
		t.Fatal("needs backfill = true, want false")
	}
}

func TestNeedsCloudCompletionBackfill_NonTerminal(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`,
		StatusRunning, int64(100), jobID,
	); err != nil {
		t.Fatalf("seed running attempt: %v", err)
	}

	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if needs {
		t.Fatal("needs backfill = true, want false")
	}
}

func TestRecordCloudJobCompletion_ZeroEndTime(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	// Record completion with endTimeUnix=0 (unknown)
	before := time.Now().Unix()
	if _, err := RecordCloudJobCompletion(database, jobID, 0, 100, 0, ""); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}
	after := time.Now().Unix()

	var endTime int64
	if err := database.QueryRow(
		`SELECT end_time FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&endTime); err != nil {
		t.Fatalf("query end_time: %v", err)
	}
	if endTime == 0 {
		t.Fatal("end_time = 0, want non-zero (should fall back to current time)")
	}
	if endTime < before || endTime > after {
		t.Fatalf("end_time = %d, want between %d and %d", endTime, before, after)
	}
}
