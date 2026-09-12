package db

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The --if-online cleanup path deletes a freshly submitted record when the
// host does not acknowledge dispatch. A bare DELETE FROM jobs died on a
// foreign-key violation once the job had execution history (wb131).
func TestDeleteQueuedJobIfNeverStarted(t *testing.T) {
	t.Run("deletes a fresh queued job and its submission children", func(t *testing.T) {
		database := SetupTestDB(t)
		jobID, err := RecordQueued(database, "host-alpha", "/tmp/project", "echo hi", "fresh")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := InsertJobPayload(database, JobPayload{
			JobID: jobID, Name: "execution-prompt", StoredPath: "payloads/hash",
			SizeBytes: 3, SHA256: "ab", R2Key: "assets/hash",
		}); err != nil {
			t.Fatalf("InsertJobPayload: %v", err)
		}
		// Telemetry rows have a non-CASCADE FK to jobs; they must go before
		// the parent or the delete dies exactly like wb131's cleanup did.
		if _, err := database.Exec(`INSERT INTO placement_decisions (job_id, decision_kind, created_at) VALUES (?, 'run', strftime('%s','now'))`, jobID); err != nil {
			t.Fatalf("insert placement decision: %v", err)
		}

		if err := DeleteQueuedJobIfNeverStarted(database, jobID); err != nil {
			t.Fatalf("DeleteQueuedJobIfNeverStarted: %v", err)
		}
		if job, _ := GetJobByID(database, jobID); job != nil {
			t.Fatal("job row survived deletion")
		}
		var payloads int
		if err := database.QueryRow(`SELECT COUNT(*) FROM job_payloads WHERE job_id = ?`, jobID).Scan(&payloads); err != nil {
			t.Fatal(err)
		}
		if payloads != 0 {
			t.Fatalf("payload rows survived deletion: %d", payloads)
		}
	})

	t.Run("refuses a job whose attempt started executing", func(t *testing.T) {
		database := SetupTestDB(t)
		jobID, err := RecordQueued(database, "host-alpha", "/tmp/project", "echo hi", "started")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if _, err := CreateAttempt(database, jobID, "host-alpha", nil, StatusCompleted); err != nil {
			t.Fatalf("CreateAttempt: %v", err)
		}
		if err := UpdateStartTime(database, jobID, time.Now().Unix()); err != nil {
			t.Fatalf("UpdateStartTime: %v", err)
		}

		err = DeleteQueuedJobIfNeverStarted(database, jobID)
		if !errors.Is(err, ErrJobAlreadyStarted) {
			t.Fatalf("DeleteQueuedJobIfNeverStarted = %v, want ErrJobAlreadyStarted", err)
		}
		if job, _ := GetJobByID(database, jobID); job == nil {
			t.Fatal("started job row was deleted")
		}
		var attempts int
		if err := database.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id = ?`, jobID).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts == 0 {
			t.Fatal("started job lost its attempt rows")
		}
	})

	t.Run("refuses a job that is not queued or draft", func(t *testing.T) {
		database := SetupTestDB(t)
		jobID, err := RecordQueued(database, "host-alpha", "/tmp/project", "echo hi", "running")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := UpdateQueuedToRunning(database, jobID); err != nil {
			t.Fatalf("UpdateQueuedToRunning: %v", err)
		}

		err = DeleteQueuedJobIfNeverStarted(database, jobID)
		if err == nil || !strings.Contains(err.Error(), "started") {
			t.Fatalf("DeleteQueuedJobIfNeverStarted = %v, want started refusal", err)
		}
		if job, _ := GetJobByID(database, jobID); job == nil {
			t.Fatal("running job row was deleted")
		}
	})
}
