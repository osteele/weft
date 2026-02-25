package ops

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
)

func TestApplyPauseToRemote_CreatesMarkerFile(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "pause test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// Track which commands are called
	pausedFile := session.SimplePausedFile(job.ID)
	var touchCalled, killCalled bool

	mockSSHFunc(t, func(host, cmd string) (string, string, int) {
		if strings.Contains(cmd, "touch") && strings.Contains(cmd, pausedFile) {
			touchCalled = true
			return "", "", 0
		}
		if strings.Contains(cmd, "kill -STOP") {
			killCalled = true
			return "", "", 0
		}
		return "", "", 0
	})

	err = applyPauseToRemote(job, time.Second)
	if err != nil {
		t.Fatalf("applyPauseToRemote: %v", err)
	}

	if !touchCalled {
		t.Error("expected touch command for .paused marker file to be called")
	}
	if !killCalled {
		t.Error("expected kill -STOP command to be called")
	}
}

func TestApplyResumeToRemote_RemovesMarkerFile(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "resume test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	pausedFile := session.SimplePausedFile(job.ID)
	var killCalled, rmCalled bool

	mockSSHFunc(t, func(host, cmd string) (string, string, int) {
		if strings.Contains(cmd, "kill -CONT") {
			killCalled = true
			return "", "", 0
		}
		if strings.Contains(cmd, "rm -f") && strings.Contains(cmd, pausedFile) {
			rmCalled = true
			return "", "", 0
		}
		return "", "", 0
	})

	err = applyResumeToRemote(job, time.Second)
	if err != nil {
		t.Fatalf("applyResumeToRemote: %v", err)
	}

	if !killCalled {
		t.Error("expected kill -CONT command to be called")
	}
	if !rmCalled {
		t.Error("expected rm command for .paused marker file to be called")
	}
}

func TestApplyPauseToRemote_SlurmNotSupported(t *testing.T) {
	// Create a mock job with SLURM backend set
	job := &db.Job{
		ID:      1,
		Host:    "test-host",
		Backend: db.BackendSlurm, // This makes UsesSlurm() return true
	}

	err := applyPauseToRemote(job, time.Second)
	if err == nil {
		t.Error("expected error for SLURM job pause")
	}
	if err != nil && !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected 'not supported' error, got: %v", err)
	}
}

func TestApplyPauseToRemote_ConnectionError(t *testing.T) {
	// Verify that SSH connection errors are properly detected and reported
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "pause conn test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// Simulate SSH connection error: the error is "exit status 255" but
	// the actual connection message is in stderr
	mockSSHFunc(t, func(host, cmd string) (string, string, int) {
		return "", "Connection closed by 10.1.2.3 port 22", 255
	})

	err = applyPauseToRemote(job, time.Second)
	if err == nil {
		t.Error("expected error for connection failure")
	}
	// The error should contain the connection error message, not just "exit status 255"
	if err != nil && !strings.Contains(err.Error(), "connection error") {
		t.Errorf("expected 'connection error' in error, got: %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "Connection closed") {
		t.Errorf("expected stderr message in error, got: %v", err)
	}
}

func TestApplyResumeToRemote_SlurmNotSupported(t *testing.T) {
	// Create a mock job with SLURM backend set
	job := &db.Job{
		ID:      1,
		Host:    "test-host",
		Backend: db.BackendSlurm, // This makes UsesSlurm() return true
	}

	err := applyResumeToRemote(job, time.Second)
	if err == nil {
		t.Error("expected error for SLURM job resume")
	}
	if err != nil && !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected 'not supported' error, got: %v", err)
	}
}

func TestApplyStartToRemote_PreservesPendingOnFailure(t *testing.T) {
	// Verify that when starting a draft job fails (e.g., SSH error),
	// the pending_status is preserved for retry.
	database := db.SetupTestDB(t)

	// Create a draft job with pending_status=running
	jobID, err := db.RecordDraftJob(database, "test-host", "/tmp", "sleep 100", "start fail test", "", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetPendingStatus(database, jobID, db.StatusRunning); err != nil {
		t.Fatalf("set pending status: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// First call succeeds (queue job), second call fails (start job)
	callCount := 0
	mockSSHFunc(t, func(host, cmd string) (string, string, int) {
		callCount++
		if callCount <= 2 {
			// First two calls: queue-related commands succeed
			return "", "", 0
		}
		// Third call (startJobFromRecord) fails with connection error
		return "", "Connection closed by remote host", 255
	})

	err = applyStartToRemote(database, job, time.Second)
	if err == nil {
		t.Fatal("expected error from applyStartToRemote")
	}

	// Verify pending_status is still set (for retry)
	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if updated.PendingStatus == nil {
		t.Error("pending_status should be preserved when start fails, but it was cleared")
	} else if *updated.PendingStatus != db.StatusRunning {
		t.Errorf("pending_status should be 'running', got '%s'", *updated.PendingStatus)
	}
}

func TestReconcile_RequeueOverTerminalRemote(t *testing.T) {
	// When a job is requeued (pending_status=queued) but the remote still has
	// a terminal status from the previous run, reconcile should apply the
	// requeue rather than accepting the stale terminal state.
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "requeue test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Simulate: job was killed, then requeued
	if err := db.RequeueByID(database, jobID); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// Verify requeue set pending_status
	if job.PendingStatus == nil || *job.PendingStatus != db.StatusQueued {
		t.Fatalf("expected pending_status=queued after requeue, got %v", job.PendingStatus)
	}

	// Mock SSH: queue append succeeds
	mockSSHFunc(t, func(host, cmd string) (string, string, int) {
		return "", "", 0
	})

	// Remote reports "completed" (stale status file from previous run)
	result, err := Reconcile(database, job, db.StatusCompleted, ReconcileOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Should have applied the requeue, not accepted the terminal state
	if result.NewStatus != db.StatusQueued {
		t.Errorf("expected status queued after reconcile, got %s", result.NewStatus)
	}
	if !result.Conflict {
		t.Error("expected conflict flag to be set")
	}
	if !strings.Contains(result.Resolution, "requeued over stale terminal") {
		t.Errorf("expected requeue resolution, got: %s", result.Resolution)
	}

	// Verify the job status in DB
	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if updated.Status != db.StatusQueued {
		t.Errorf("expected job status queued in DB, got %s", updated.Status)
	}
	if updated.PendingStatus != nil {
		t.Errorf("expected pending_status cleared after successful requeue, got %v", *updated.PendingStatus)
	}
}

func TestReconcile_NoPendingAcceptsTerminalRemote(t *testing.T) {
	// When there's no local intent (no pending_status), a terminal remote
	// state should be accepted normally (this is the pre-existing behavior).
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "terminal test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	// Set last_synced_status so base != remote triggers Case 2
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusRunning); err != nil {
		t.Fatalf("update last synced: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// No pending status — pure sync
	result, err := Reconcile(database, job, db.StatusCompleted, ReconcileOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.NewStatus != db.StatusCompleted {
		t.Errorf("expected status completed, got %s", result.NewStatus)
	}
}

func TestSignalJobProcess_PIDFallbackSignalsProcessGroup(t *testing.T) {
	// Verify that when falling back to PID (no PGID file), we still signal
	// the process group by looking up the PGID via ps command.
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "pgid lookup test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	var capturedCmd string
	mockSSHFunc(t, func(host, cmd string) (string, string, int) {
		capturedCmd = cmd
		return "", "", 0
	})

	err = signalJobProcess(job, "STOP", time.Second)
	if err != nil {
		t.Fatalf("signalJobProcess: %v", err)
	}

	// Verify the command includes ps -o pgid= to lookup the process group
	if !strings.Contains(capturedCmd, "ps -o pgid= -p $pid") {
		t.Errorf("expected command to lookup PGID via ps, got: %s", capturedCmd)
	}

	// Verify the command signals the process group (negative PGID)
	if !strings.Contains(capturedCmd, "kill -STOP -$pgid") {
		t.Errorf("expected command to signal process group with -$pgid, got: %s", capturedCmd)
	}
}
