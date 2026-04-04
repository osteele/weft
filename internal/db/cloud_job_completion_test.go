package db

import "testing"

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
