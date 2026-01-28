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
