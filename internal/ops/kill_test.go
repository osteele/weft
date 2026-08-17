package ops

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestKillJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	mockSSHCommands(t, []sshMockResponse{
		// ProbeRemoteStatus (TmuxSessionExistsQuickTimeout)
		{Contains: "tmux has-session", Stdout: "NO\n"},
		// applyKillToRemote
		{Contains: "tmux kill-session", Stdout: ""},
	})

	result, err := KillJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("KillJob failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Deferred {
		t.Error("expected Deferred to be false")
	}
	if result.Message != "Job wj1 killed" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status != db.StatusKilled {
		t.Errorf("expected job status to be killed, got %s", updatedJob.Status)
	}
}

func TestKillJob_QuickTimeout(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	// Mock immediate connection failure (no sync attempted)
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := KillJob(database, job, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("KillJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}
	if result.Message != "Job wj1 kill pending (host unreachable)" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.EffectiveStatus() != db.StatusKilled {
		t.Errorf("expected effective status killed, got %s", updatedJob.EffectiveStatus())
	}
	if updatedJob.PendingStatus == nil || *updatedJob.PendingStatus != db.StatusKilled {
		t.Errorf("expected pending status to be killed, got %v", updatedJob.PendingStatus)
	}
}

func TestKillJob_SyncError(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	// Mock: probe succeeds (session exists), then kill fails with connection error
	callCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		callCount++
		if callCount == 1 {
			return "", "", 0 // tmux has-session succeeds (session exists)
		}
		// Kill command fails (connection dropped during apply)
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := KillJob(database, job, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("KillJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.PendingStatus == nil || *updatedJob.PendingStatus != db.StatusKilled {
		t.Errorf("expected pending status to be killed, got %v", updatedJob.PendingStatus)
	}
}

// TestStopJob_CancelRunningJobRecordsCanceled is a regression test:
// `weft cancel` on a running job must record status canceled, not killed.
func TestStopJob_CancelRunningJobRecordsCanceled(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	// Make this a tmux-session job (not queue-runner) so the cancel apply
	// path must kill the tmux session.
	if _, err := database.Exec(`UPDATE job_attempts SET session_name = ? WHERE job_id = ?`, "test-session", jobID); err != nil {
		t.Fatalf("set attempt session_name: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)
	if job.UsesQueueRunner() {
		t.Fatal("expected a tmux-session job, got queue-runner job")
	}

	var capturedCommands []string
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		capturedCommands = append(capturedCommands, command)
		if strings.Contains(command, "tmux has-session") {
			return "NO\n", "", 0
		}
		return "", "", 0
	})

	result, err := StopJob(database, job, db.StatusCanceled, DefaultOptions())
	if err != nil {
		t.Fatalf("StopJob failed: %v", err)
	}
	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Message != "Job wj1 canceled" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status != db.StatusCanceled {
		t.Errorf("expected job status canceled, got %s", updatedJob.Status)
	}
	if updatedJob.EffectiveStatus() != db.StatusCanceled {
		t.Errorf("expected effective status canceled, got %s", updatedJob.EffectiveStatus())
	}

	// The running process must still be stopped: tmux-session jobs need a
	// tmux kill-session, which the cancel apply path previously skipped.
	hasSessionKill := false
	for _, cmd := range capturedCommands {
		if strings.Contains(cmd, "tmux kill-session") {
			hasSessionKill = true
		}
	}
	if !hasSessionKill {
		t.Errorf("expected tmux kill-session to be issued; got commands: %v", capturedCommands)
	}
}

// TestStopJob_CancelPausedJobRecordsCanceled is a regression test:
// `weft cancel` on a paused job must record canceled, not be converted to a kill.
func TestStopJob_CancelPausedJobRecordsCanceled(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	if err := db.MarkPausedByID(database, jobID); err != nil {
		t.Fatalf("MarkPausedByID: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)
	if job.Status != db.StatusPaused {
		t.Fatalf("expected paused job, got %s", job.Status)
	}

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "tmux has-session") {
			return "NO\n", "", 0
		}
		return "", "", 0
	})

	result, err := StopJob(database, job, db.StatusCanceled, DefaultOptions())
	if err != nil {
		t.Fatalf("StopJob failed: %v", err)
	}
	if !result.Success {
		t.Error("expected Success to be true")
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status != db.StatusCanceled {
		t.Errorf("expected job status canceled, got %s", updatedJob.Status)
	}
	if updatedJob.EffectiveStatus() != db.StatusCanceled {
		t.Errorf("expected effective status canceled, got %s", updatedJob.EffectiveStatus())
	}
}

func TestCancelQueuedJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test")
	job, _ := db.GetJobByID(database, jobID)

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "jq -e") {
			return "YES\n", "", 0
		}
		return "", "", 0
	})

	result, err := CancelQueuedJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("CancelQueuedJob failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Deferred {
		t.Error("expected Deferred to be false")
	}
	if result.Message != "Job wj1 canceled" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status != db.StatusCanceled {
		t.Errorf("expected job status to be canceled, got %s", updatedJob.Status)
	}
	if updatedJob.PendingStatus != nil {
		t.Errorf("expected pending status to be nil, got %v", updatedJob.PendingStatus)
	}
}

func TestCancelQueuedJob_ClearsQueuedIntentForStatusView(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test")
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusQueued, jobID); err != nil {
		t.Fatalf("set requested_status queued: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "jq -e") {
			return "YES\n", "", 0
		}
		return "", "", 0
	})

	result, err := CancelQueuedJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("CancelQueuedJob failed: %v", err)
	}
	if !result.Success || result.Deferred {
		t.Fatalf("unexpected result: %+v", result)
	}

	updatedJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID(updated): %v", err)
	}
	if updatedJob.EffectiveStatus() != db.StatusCanceled {
		t.Fatalf("expected effective status canceled, got %s", updatedJob.EffectiveStatus())
	}
	if updatedJob.Status != db.StatusCanceled {
		t.Fatalf("expected status canceled, got %s", updatedJob.Status)
	}
}

func TestCancelQueuedJob_QuickTimeout(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test")
	job, _ := db.GetJobByID(database, jobID)

	// Mock immediate connection failure (no sync attempted)
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := CancelQueuedJob(database, job, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("CancelQueuedJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true on pending cancel")
	}
	if result.Message != "Job wj1 cancel pending (host unreachable)" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.EffectiveStatus() != db.StatusCanceled {
		t.Errorf("expected effective status canceled, got %s", updatedJob.EffectiveStatus())
	}
	if updatedJob.PendingStatus == nil || *updatedJob.PendingStatus != db.StatusCanceled {
		t.Errorf("expected pending status to be canceled, got %v", updatedJob.PendingStatus)
	}
}

func TestCancelQueuedJob_UnplacedNoAttempt(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.TargetKind() != db.JobTargetUnplaced {
		t.Fatalf("expected unplaced job, got %s", job.TargetKind())
	}

	result, err := CancelQueuedJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("CancelQueuedJob failed: %v", err)
	}
	if !result.Success {
		t.Fatal("expected Success to be true")
	}
	if result.Deferred {
		t.Fatal("expected Deferred to be false")
	}
	// The message must not claim more than a local write achieved. A cancel
	// cannot recall a launch already dispatched, and reporting a bare
	// "canceled" is what stopped a user checking while the job ran anyway
	// (wb72).
	if !strings.Contains(result.Message, "canceled locally") {
		t.Fatalf("message should scope the cancel to local state: %q", result.Message)
	}
	if !strings.Contains(result.Message, "wj1") {
		t.Fatalf("message should name the job: %q", result.Message)
	}

	updatedJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID(updated): %v", err)
	}
	if updatedJob.Status != db.StatusCanceled {
		t.Fatalf("expected status canceled, got %s", updatedJob.Status)
	}
	if updatedJob.EffectiveStatus() != db.StatusCanceled {
		t.Fatalf("expected effective status canceled, got %s", updatedJob.EffectiveStatus())
	}
}

func TestUnplaceQueuedJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test")
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("UpdateLastSyncedStatus: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "", 0
	})

	result, err := UnplaceQueuedJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("UnplaceQueuedJob failed: %v", err)
	}
	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Deferred {
		t.Error("expected Deferred to be false")
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Host != "" {
		t.Errorf("expected job host to be empty, got %q", updatedJob.Host)
	}
	if updatedJob.Status != db.StatusQueued {
		t.Errorf("expected job status queued, got %s", updatedJob.Status)
	}
	if updatedJob.LastSyncedStatus != "" {
		t.Errorf("expected last synced status to be empty, got %q", updatedJob.LastSyncedStatus)
	}
	if !updatedJob.HasTag(db.TagCloud) {
		t.Errorf("expected job to gain cloud tag, got %v", updatedJob.Tags)
	}
	pending, err := db.HasPendingOperation(database, jobID, db.OpRemoveQueued)
	if err != nil {
		t.Fatalf("HasPendingOperation: %v", err)
	}
	if pending {
		t.Error("expected remove_queued deferred op to be cleared")
	}
}

func TestUnplaceQueuedJob_DeferredWhenHostOffline(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test")
	job, _ := db.GetJobByID(database, jobID)

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := UnplaceQueuedJob(database, job, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("UnplaceQueuedJob returned an unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected Success to be true")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Host != "" {
		t.Errorf("expected job host to be empty, got %q", updatedJob.Host)
	}
	if !updatedJob.HasTag(db.TagCloud) {
		t.Errorf("expected job to gain cloud tag, got %v", updatedJob.Tags)
	}
	pending, err := db.HasPendingOperation(database, jobID, db.OpRemoveQueued)
	if err != nil {
		t.Fatalf("HasPendingOperation: %v", err)
	}
	if !pending {
		t.Error("expected remove_queued deferred op to remain pending")
	}
}

func TestUnplaceQueuedJob_CloudJob(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a cloud instance
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Create an unplaced job, then assign it to the cloud instance
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	if !job.IsRentalJob() {
		t.Fatalf("expected job to be rental, got target kind %s", job.TargetKind())
	}

	result, err := UnplaceQueuedJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("UnplaceQueuedJob failed: %v", err)
	}
	if !result.Success {
		t.Error("expected Success to be true")
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.TargetKind() != db.JobTargetUnplaced {
		t.Errorf("expected job target kind unplaced, got %s", updatedJob.TargetKind())
	}
	if updatedJob.EffectiveStatus() != db.StatusQueued {
		t.Errorf("expected job status queued, got %s", updatedJob.EffectiveStatus())
	}
}

// Note: CancelQueuedJob only makes a single SSH call (removeFromQueueFile),
// so there's no separate "SyncError" case distinct from QuickTimeout.
// The connection error case is already covered by TestCancelQueuedJob_QuickTimeout.

func TestCancelQueuedJob_NotQueued(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	_, err := CancelQueuedJob(database, job, DefaultOptions())
	if err == nil {
		t.Fatal("expected error for non-queued job")
	}
}

// TestCancelQueuedJob_UsesEffectiveStatus verifies that CancelQueuedJob checks
// EffectiveStatus() rather than raw Status, so a queued job with pending=killed
// is treated as effectively killed (not cancellable).
func TestCancelQueuedJob_UsesEffectiveStatus(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test")

	// Set pending status to killed - the job is still Status=queued but EffectiveStatus=killed
	if err := db.SetPendingStatus(database, jobID, db.StatusKilled); err != nil {
		t.Fatalf("SetPendingStatus failed: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	// Verify the job's raw status is still queued
	if job.Status != db.StatusQueued {
		t.Fatalf("expected Status to be queued, got %s", job.Status)
	}

	// Verify EffectiveStatus returns the pending status
	if job.EffectiveStatus() != db.StatusKilled {
		t.Fatalf("expected EffectiveStatus to be killed, got %s", job.EffectiveStatus())
	}

	// CancelQueuedJob should reject because EffectiveStatus is not queued
	_, err := CancelQueuedJob(database, job, DefaultOptions())
	if err == nil {
		t.Fatal("expected error for job with EffectiveStatus != queued")
	}
	if !errors.Is(err, ErrNotQueued) {
		t.Errorf("expected ErrNotQueued, got: %v", err)
	}
}

// TestKillQueueRunnerJob_TildeNotSingleQuoted verifies that the kill command
// for queue-runner jobs doesn't use single-quoted tilde paths, which would
// prevent shell expansion.
func TestKillQueueRunnerJob_TildeNotSingleQuoted(t *testing.T) {
	database := db.SetupTestDB(t)
	// Create a queue-runner job (no session name, has queue name)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test")
	// Transition to running (simulating queue runner starting it)
	db.MarkQueuedJobRunning(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	var capturedCommands []string
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		capturedCommands = append(capturedCommands, command)
		return "", "", 0
	})

	_, err := KillJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("KillJob failed: %v", err)
	}

	// Check that no command contains single-quoted tilde path
	for _, cmd := range capturedCommands {
		if strings.Contains(cmd, "'~") {
			t.Errorf("Command contains single-quoted tilde (prevents shell expansion): %s", cmd)
		}
	}
}

// TestReconcileCancelKillsRunningProcess verifies that reconciling a job with
// pending=canceled issues both a cancel command and kills any running process.
// This handles the race condition where the queue runner starts the job
// between when we set pending status and when we reconcile.
func TestReconcileCancelKillsRunningProcess(t *testing.T) {
	database := db.SetupTestDB(t)
	// Create a queued job
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test")

	// Set pending status to canceled (simulating user requesting cancel)
	if err := db.SetPendingStatus(database, jobID, db.StatusCanceled); err != nil {
		t.Fatalf("SetPendingStatus failed: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	var capturedCommands []string
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		capturedCommands = append(capturedCommands, command)
		return "", "", 0
	})

	// Use production path: SyncAndReconcile
	_, err := SyncAndReconcile(database, job, ReconcileOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("SyncAndReconcile failed: %v", err)
	}

	// Verify both cancel command AND kill commands were issued
	hasCancelCommand := false
	hasKillCommand := false
	for _, cmd := range capturedCommands {
		// Cancel command appended to .commands file
		if strings.Contains(cmd, ".commands") && strings.Contains(cmd, "cancel") {
			hasCancelCommand = true
		}
		if strings.Contains(cmd, "kill") && strings.Contains(cmd, ".pid") {
			hasKillCommand = true
		}
	}

	if !hasCancelCommand {
		t.Errorf("expected cancel command to be issued; got commands: %v", capturedCommands)
	}
	if !hasKillCommand {
		t.Error("expected kill command to be issued (for case where job started running)")
	}

	// Verify job status was updated
	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status != db.StatusCanceled {
		t.Errorf("expected job status to be canceled, got %s", updatedJob.Status)
	}
}
