package ops

import (
	"fmt"
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
)

func TestRequestStatus_ToDraft(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a queued job
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "echo test", "test job")
	db.MarkQueuedByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	// Mock SSH to succeed
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "grep", Stdout: "NO"}, // Not in queue
		{Contains: "mkdir", Stdout: ""},  // Lock successful
		{Contains: "kill", Stdout: ""},   // Kill successful
		{Contains: "tmux", Stdout: ""},   // No tmux session
	})

	result, err := RequestStatus(database, job, db.StatusDraft, TimeoutFast)
	if err != nil {
		t.Fatalf("RequestStatus failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
}

func TestRequestStatus_ToQueued(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a draft job
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "echo test", "test job")
	db.ClearPendingAndUpdateStatus(database, jobID, db.StatusDraft)
	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE jobs SET session_name = ?, start_time = ?, end_time = ?, exit_code = ?, error_message = ? WHERE id = ?`,
		fmt.Sprintf("rj-%d", jobID), now-100, now, 1, "boom", jobID,
	); err != nil {
		t.Fatalf("seed run metadata: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)

	// Mock SSH to succeed
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},   // Queue append
		{Contains: "flock", Stdout: ""},   // Fallback lock
		{Contains: "cat", Stdout: ""},     // Current file check
		{Contains: "grep", Stdout: "YES"}, // In queue
	})

	result, err := RequestStatus(database, job, db.StatusQueued, TimeoutFast)
	if err != nil {
		t.Fatalf("RequestStatus failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusQueued {
		t.Fatalf("expected queued status, got %s", updated.Status)
	}
	if updated.SessionName != "" || updated.StartTime != 0 || updated.EndTime != nil || updated.ExitCode != nil || updated.ErrorMessage != "" {
		t.Fatalf("expected run metadata cleared on re-queue, got session=%q start=%d end=%v exit=%v err=%q",
			updated.SessionName, updated.StartTime, updated.EndTime, updated.ExitCode, updated.ErrorMessage)
	}
}

func TestRequestStatus_ConnectionError(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a queued job
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "echo test", "test job")
	db.MarkQueuedByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	// Mock SSH to return connection error
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := RequestStatus(database, job, db.StatusDraft, TimeoutFast)
	if err != nil {
		t.Fatalf("RequestStatus returned unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true (deferred)")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}

	// Verify pending status was set
	updated, _ := db.GetJobByID(database, jobID)
	if updated.PendingStatus == nil || *updated.PendingStatus != db.StatusDraft {
		t.Errorf("expected pending status to be draft, got %v", updated.PendingStatus)
	}
}

func TestRequestStatus_TimeoutModes(t *testing.T) {
	tests := []struct {
		mode            TimeoutMode
		expectedTimeout time.Duration
	}{
		{TimeoutFast, 5 * time.Second},
		{TimeoutNormal, 30 * time.Second},
		{TimeoutSync, 2 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.mode.String(), func(t *testing.T) {
			opts := OptionsForMode(tt.mode)
			if opts.Timeout != tt.expectedTimeout {
				t.Errorf("OptionsForMode(%v) timeout = %v, want %v", tt.mode, opts.Timeout, tt.expectedTimeout)
			}
		})
	}
}

func TestRequestStatus_NilJob(t *testing.T) {
	database := db.SetupTestDB(t)

	_, err := RequestStatus(database, nil, db.StatusDraft, TimeoutFast)
	if err == nil {
		t.Error("expected error for nil job")
	}
}
