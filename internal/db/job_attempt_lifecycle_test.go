package db

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/status"
)

// TestMarkDeadByID_NoOpenAttemptIsNoop proves that MarkDeadByID does not mutate
// state when the latest attempt is already closed. This is the behavior the
// randomized state-machine test relies on.
func TestMarkDeadByID_NoOpenAttemptIsNoop(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(5001)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusFailed)

	if err := MarkDeadByID(database, jobID); err != nil {
		t.Fatalf("MarkDeadByID: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", job.Status, StatusFailed)
	}
	attempts, err := ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	if attempts[0].Status != StatusFailed {
		t.Fatalf("attempt status = %q, want %q", attempts[0].Status, StatusFailed)
	}
}

// TestMarkDeadByID_TransitionsOpenAttempt proves MarkDeadByID closes an open
// attempt when the transition is valid.
func TestMarkDeadByID_TransitionsOpenAttempt(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(5002)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusRunning)

	if err := MarkDeadByID(database, jobID); err != nil {
		t.Fatalf("MarkDeadByID: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", job.Status, StatusFailed)
	}
	if job.LastSyncedStatus != StatusFailed {
		t.Fatalf("last_synced_status = %q, want %q", job.LastSyncedStatus, StatusFailed)
	}
}

// TestRecordCompletionByID_AuthoritativeOverride proves a failed attempt can be
// overwritten by a later authoritative completion marker.
func TestRecordCompletionByID_AuthoritativeOverride(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(5003)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusFailed)

	now := time.Now().Unix()
	if err := RecordCompletionByID(database, jobID, 0, now); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q", job.Status, StatusCompleted)
	}
	if job.LastSyncedStatus != StatusCompleted {
		t.Fatalf("last_synced_status = %q, want %q", job.LastSyncedStatus, StatusCompleted)
	}
}

// TestRecordCompletionByID_NoAttemptIsNoop proves RecordCompletionByID returns
// without error and without mutating the jobs row when there are no attempts.
func TestRecordCompletionByID_NoAttemptIsNoop(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(5004)
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, command, requested_status) VALUES (?, ?, ?, ?)`,
		jobID, "/tmp", "echo test", StatusQueued,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	now := time.Now().Unix()
	if err := RecordCompletionByID(database, jobID, 0, now); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("status = %q, want %q", job.Status, StatusQueued)
	}
	attempts, err := ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempts = %d, want 0", len(attempts))
	}
}

// TestRecordCompletionByID_RejectInvalidTransition proves RecordCompletionByID
// returns an error when the transition would be invalid even authoritatively.
func TestRecordCompletionByID_RejectInvalidTransition(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(5005)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusDraft)

	now := time.Now().Unix()
	err := RecordCompletionByID(database, jobID, 0, now)
	if err == nil {
		t.Fatal("RecordCompletionByID returned nil, want error")
	}
	if _, ok := err.(*status.InvalidTransitionError); !ok {
		t.Fatalf("error type = %T, want *status.InvalidTransitionError", err)
	}
}

// TestMarkQueuedJobRunning_NoOpenAttemptIsNoop proves MarkQueuedJobRunning does
// not create or mutate attempts when there is no open attempt.
func TestMarkQueuedJobRunning_NoOpenAttemptIsNoop(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(5006)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusCompleted)

	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q", job.Status, StatusCompleted)
	}
	attempts, err := ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
}

// TestMarkQueuedJobStarting_NoOpenAttemptIsNoop proves MarkQueuedJobStarting
// does not create or mutate attempts when there is no open attempt.
func TestMarkQueuedJobStarting_NoOpenAttemptIsNoop(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(5007)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusCompleted)

	if err := MarkQueuedJobStarting(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobStarting: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q", job.Status, StatusCompleted)
	}
}

// TestMarkQueuedByID_ClosedOnPremAttemptCreatesNewAttempt proves that
// MarkQueuedByID creates a fresh queued attempt when the latest closed attempt
// has no cloud launch_id (legacy on-prem requeue).
func TestMarkQueuedByID_ClosedOnPremAttemptCreatesNewAttempt(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(5008)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusFailed)

	if err := MarkQueuedByID(database, jobID); err != nil {
		t.Fatalf("MarkQueuedByID: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("status = %q, want %q", job.Status, StatusQueued)
	}
	attempts, err := ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	if attempts[0].Status != StatusQueued {
		t.Fatalf("latest attempt status = %q, want %q", attempts[0].Status, StatusQueued)
	}
	if attempts[0].EndTime != nil {
		t.Fatal("new queued attempt has end_time set")
	}
}

// TestMarkQueuedByID_ClosedCloudAttemptIsNoop proves that MarkQueuedByID does
// not create a new attempt for a closed cloud attempt, because the closed cloud
// row is a historical fact.
func TestMarkQueuedByID_ClosedCloudAttemptIsNoop(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(5009)
	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusFailed, withLaunch(launchID))

	if err := MarkQueuedByID(database, jobID); err != nil {
		t.Fatalf("MarkQueuedByID: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", job.Status, StatusFailed)
	}
	attempts, err := ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
}
