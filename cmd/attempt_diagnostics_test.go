package cmd

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

func createUnstartedRetryWithFailedCloudAttempt(t *testing.T, database *sql.DB) (*db.Job, db.JobAttempt) {
	t.Helper()
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	exitCode := 1
	if err := db.CloseAttempt(database, jobID, db.StatusFailed, &exitCode, time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	if err := db.CloseLaunchAttempt(database, jobID, db.AttemptOutcomeFailed); err != nil {
		t.Fatalf("CloseLaunchAttempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusQueued, jobID); err != nil {
		t.Fatalf("set requested_status queued: %v", err)
	}

	attempts, err := db.ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	failed := attempts[0]
	if failed.StartTime == nil {
		t.Fatal("failed cloud attempt has no start time")
	}
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt queued retry: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	return job, failed
}

func TestAttemptDisplayProvenanceFindsLatestStartedAttempt(t *testing.T) {
	database := db.SetupTestDB(t)
	job, failed := createUnstartedRetryWithFailedCloudAttempt(t, database)

	provenance, err := loadAttemptDisplayProvenance(database, job)
	if err != nil {
		t.Fatalf("loadAttemptDisplayProvenance: %v", err)
	}
	if !provenance.currentUnstarted {
		t.Fatal("currentUnstarted = false, want true")
	}
	if provenance.latestStarted == nil || provenance.latestStarted.ID != failed.ID {
		t.Fatalf("latestStarted = %#v, want attempt ID %d", provenance.latestStarted, failed.ID)
	}
}

func TestRunLogForJobShowsLatestStartedAttemptForUnstartedRetry(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	database := db.SetupTestDB(t)
	job, failed := createUnstartedRetryWithFailedCloudAttempt(t, database)

	originalFetch := fetchAndDisplayLogFromR2Func
	defer func() { fetchAndDisplayLogFromR2Func = originalFetch }()
	var fetchedRunID int64
	fetchAndDisplayLogFromR2Func = func(_ *cobra.Command, _ *sql.DB, _ *db.Job, runID int64) error {
		fetchedRunID = runID
		fmt.Println("attempt log")
		return nil
	}

	cmd := &cobra.Command{Use: "log"}
	addLogFlags(cmd)
	out := captureStdout(t, func() {
		if err := runLogForJob(cmd, database, job.ID); err != nil {
			t.Fatalf("runLogForJob: %v", err)
		}
	})
	if fetchedRunID != failed.ID {
		t.Fatalf("fetched run ID = %d, want failed attempt ID %d", fetchedRunID, failed.ID)
	}
	if !strings.Contains(out, fmt.Sprintf("Current retry has not started; showing attempt #%d", failed.AttemptNumber)) {
		t.Fatalf("output = %q, want selected-attempt notice", out)
	}
	if !strings.Contains(out, "attempt log") {
		t.Fatalf("output = %q, want fetched attempt log", out)
	}
	if !logFull || logFrom != 1 {
		t.Fatalf("default range = full:%t from:%d, want full failed-attempt log", logFull, logFrom)
	}
}

func TestAttemptProvenanceSurfacesExplicitLogCommand(t *testing.T) {
	database := db.SetupTestDB(t)
	job, failed := createUnstartedRetryWithFailedCloudAttempt(t, database)

	statusOut := captureStdout(t, func() {
		printJobStatus(database, job, false)
	})
	if !strings.Contains(statusOut, "Attempt:  current retry has not started") {
		t.Fatalf("status output = %q, want current-attempt provenance", statusOut)
	}
	if !strings.Contains(statusOut, fmt.Sprintf("Evidence: #%d failed", failed.AttemptNumber)) {
		t.Fatalf("status output = %q, want evidence-attempt provenance", statusOut)
	}

	infoOut := captureStdout(t, func() {
		printAttemptsSection(&cobra.Command{}, database, job)
	})
	wantLogCommand := fmt.Sprintf("weft log wj%d --attempt %d", job.ID, failed.AttemptNumber)
	if !strings.Contains(infoOut, wantLogCommand) {
		t.Fatalf("attempt output = %q, want %q", infoOut, wantLogCommand)
	}
}
