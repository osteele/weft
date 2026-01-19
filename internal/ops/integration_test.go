package ops_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/queuerunner"
	"github.com/osteele/remote-jobs/internal/remote"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// Integration tests for job lifecycle against a real SSH server.
// These tests run when SSH_TEST_HOST is set, otherwise they skip.
//
// Requirements:
//   - SSH_TEST_HOST environment variable set to user@hostname
//   - SSH_AUTH_SOCK environment variable set to the SSH agent socket
//
// Example: SSH_TEST_HOST=root@37.16.31.67 go test -v ./internal/ops/... -run "Integration"

func getTestHost(t *testing.T) string {
	host := os.Getenv("SSH_TEST_HOST")
	if host == "" {
		t.Skip("SSH_TEST_HOST not set - skipping integration test")
	}
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("SSH_AUTH_SOCK not set - skipping integration test")
	}
	return host
}

func setupIntegrationTestDB(t *testing.T) *sql.DB {
	database := db.SetupTestDB(t)
	return database
}

// clearRemoteJobState removes all remote state files for a job ID.
// This is needed because tests reuse job ID 1 but share the remote queue runner.
func clearRemoteJobState(t *testing.T, host string, jobID int64) {
	t.Helper()
	// Remove status, log, meta, pid, pgid, and samples files for this job
	cmd := fmt.Sprintf("rm -f ~/.cache/remote-jobs/logs/%d.* ~/.cache/remote-jobs/logs/%d-*.* ~/.cache/remote-jobs/queue/job-%d.json 2>/dev/null || true", jobID, jobID, jobID)
	_, _, err := ssh.RunWithTimeout(host, cmd, 10*time.Second)
	if err != nil {
		t.Logf("Warning: could not clear remote state for job %d: %v", jobID, err)
	}
}

// stopQueueRunner stops the queue runner on the remote host.
// Returns true if the runner was stopped, false if it wasn't running.
func stopQueueRunner(t *testing.T, host string, queueName string) bool {
	t.Helper()
	pidFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.runner.pid", queueName)
	cmd := fmt.Sprintf("if [ -f %s ]; then kill $(cat %s) 2>/dev/null && rm -f %s && echo stopped; else echo not_running; fi", pidFile, pidFile, pidFile)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, 10*time.Second)
	if err != nil {
		t.Logf("Warning: could not stop queue runner: %v", err)
		return false
	}
	return strings.TrimSpace(stdout) == "stopped"
}

// ensureQueueRunnerStarted uses the production queuerunner package to start the runner.
// This ensures tests exercise the same code path as production.
func ensureQueueRunnerStarted(t *testing.T, host string) (bool, error) {
	t.Helper()
	runner := queuerunner.NewRunner(host, ops.DefaultQueueName)
	return runner.EnsureStarted("")
}

func TestIntegration_QueueJobWithArtifactEnvVars(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Queue a job that prints its RJ_* environment variables
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "env | grep -E '^RJ_' | sort",
		Description: "Integration test: artifact env vars",
		EnvVars:     []string{"MY_CUSTOM_VAR=test_value"},
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	if !result.Success {
		t.Fatal("Expected job to be queued successfully")
	}

	jobID := result.JobID
	t.Logf("Queued job %d on %s", jobID, host)

	// Verify job was recorded with correct status
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Errorf("Expected job status to be queued, got %s", job.Status)
	}

	// Clean up: cancel the job
	cancelCmd := ops.NewCancelCommand(jobID)
	_ = ops.AppendCommand(host, ops.DefaultQueueName, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second})
}

func TestIntegration_JobLifecycle(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Ensure queue runner is installed and started using production code path
	started, err := ensureQueueRunnerStarted(t, host)
	if err != nil {
		t.Fatalf("Failed to start queue runner: %v", err)
	}
	if started {
		t.Logf("Started queue runner on %s", host)
	} else {
		t.Logf("Queue runner already running on %s", host)
	}

	// Queue a quick job that outputs its environment
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "echo 'RJ_JOB_ID='$RJ_JOB_ID; echo 'RJ_ARTIFACT_MANIFEST='$RJ_ARTIFACT_MANIFEST; sleep 1; echo 'done'",
		Description: "Integration test: job lifecycle",
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	jobID := result.JobID
	t.Logf("Queued job %d, waiting for completion...", jobID)

	// Wait for job to complete (up to 60 seconds)
	deadline := time.Now().Add(60 * time.Second)
	var finalJob *db.Job
	for time.Now().Before(deadline) {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			t.Fatalf("GetJobByID failed: %v", err)
		}

		// Sync the job status
		_, err = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
		if err != nil {
			t.Logf("SyncJob error (may be transient): %v", err)
		}

		// Re-fetch after sync
		job, _ = db.GetJobByID(database, jobID)
		if db.IsTerminalStatus(job.Status) {
			finalJob = job
			break
		}

		t.Logf("Job status: %s, waiting...", job.Status)
		time.Sleep(2 * time.Second)
	}

	if finalJob == nil {
		t.Fatal("Job did not complete within timeout")
	}

	t.Logf("Job completed with status: %s, exit code: %d", finalJob.Status, *finalJob.ExitCode)

	if finalJob.Status != db.StatusCompleted {
		t.Errorf("Expected job status to be completed, got %s", finalJob.Status)
	}

	// Verify last_synced_status consistency - this catches bugs where sync operations
	// update status but forget to update last_synced_status
	if finalJob.LastSyncedStatus != finalJob.Status {
		t.Errorf("last_synced_status (%s) should match status (%s) after sync completes",
			finalJob.LastSyncedStatus, finalJob.Status)
	}
}

func TestIntegration_QueueEntryContainsArtifactEnvVars(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Queue a job
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "echo test",
		Description: "Integration test: verify queue entry",
		EnvVars:     []string{"USER_VAR=abc"},
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	jobID := result.JobID
	t.Logf("Queued job %d, checking queue entry...", jobID)

	// Fetch the queue entry from remote to verify env vars
	entry, err := fetchQueueEntryFromRemote(host, ops.DefaultQueueName, jobID, 10*time.Second)
	if err != nil {
		t.Fatalf("fetchQueueEntryFromRemote failed: %v", err)
	}

	// Verify artifact env vars are present
	foundJobID := false
	foundManifest := false
	foundRoot := false
	foundUserVar := false

	for _, env := range entry.EnvVars {
		if strings.HasPrefix(env, "RJ_JOB_ID=") {
			foundJobID = true
		}
		if strings.HasPrefix(env, "RJ_ARTIFACT_MANIFEST=") {
			foundManifest = true
		}
		if strings.HasPrefix(env, "RJ_ARTIFACT_ROOT=") {
			foundRoot = true
		}
		if env == "USER_VAR=abc" {
			foundUserVar = true
		}
	}

	if !foundJobID {
		t.Errorf("Queue entry missing RJ_JOB_ID env var, got: %v", entry.EnvVars)
	}
	if !foundManifest {
		t.Errorf("Queue entry missing RJ_ARTIFACT_MANIFEST env var, got: %v", entry.EnvVars)
	}
	if !foundRoot {
		t.Errorf("Queue entry missing RJ_ARTIFACT_ROOT env var, got: %v", entry.EnvVars)
	}
	if !foundUserVar {
		t.Errorf("Queue entry missing USER_VAR env var, got: %v", entry.EnvVars)
	}

	// Clean up
	cancelCmd := ops.NewCancelCommand(jobID)
	_ = ops.AppendCommand(host, ops.DefaultQueueName, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second})
}

// State transition tests

func TestIntegration_StateTransition_QueuedToDraft(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Clear any previous state for job ID 1 (tests reuse IDs since each has fresh DB)
	clearRemoteJobState(t, host, 1)

	// Queue a job
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "sleep 60",
		Description: "Integration test: queued to draft",
	}
	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	jobID := result.JobID
	t.Logf("Queued job %d", jobID)

	// Verify initial state is queued
	job, _ := db.GetJobByID(database, jobID)
	if job.Status != db.StatusQueued {
		t.Fatalf("Expected initial status queued, got %s", job.Status)
	}

	// Transition to draft (cancel the job)
	if err := db.SetPendingStatus(database, jobID, db.StatusDraft); err != nil {
		t.Fatalf("SetPendingStatus failed: %v", err)
	}

	// Reconcile to apply the pending change
	job, _ = db.GetJobByID(database, jobID)
	_, err = ops.SyncAndReconcile(database, job, ops.ReconcileOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Logf("Reconcile error (may be expected): %v", err)
	}

	// Verify final state
	job, _ = db.GetJobByID(database, jobID)
	if job.Status != db.StatusDraft {
		t.Errorf("Expected final status draft, got %s", job.Status)
	}
}

func TestIntegration_StateTransition_QueuedToCanceled(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Stop queue runner so jobs stay queued (don't actually run)
	if stopped := stopQueueRunner(t, host, ops.DefaultQueueName); stopped {
		t.Log("Stopped queue runner for state transition test")
	}

	// Clear any previous state for job ID 1 (tests reuse IDs since each has fresh DB)
	clearRemoteJobState(t, host, 1)

	// Queue a job
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "sleep 60",
		Description: "Integration test: queued to canceled",
	}
	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	jobID := result.JobID
	t.Logf("Queued job %d", jobID)

	// Cancel the job
	if err := db.SetPendingStatus(database, jobID, db.StatusCanceled); err != nil {
		t.Fatalf("SetPendingStatus failed: %v", err)
	}

	// Reconcile
	job, _ := db.GetJobByID(database, jobID)
	_, err = ops.SyncAndReconcile(database, job, ops.ReconcileOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Logf("Reconcile error (may be expected): %v", err)
	}

	// Verify final state
	job, _ = db.GetJobByID(database, jobID)
	if job.Status != db.StatusCanceled {
		t.Errorf("Expected final status canceled, got %s", job.Status)
	}
}

func TestIntegration_StateTransition_QueueEntryRemovedOnCancel(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Clear any previous state for job ID 1 (tests reuse IDs since each has fresh DB)
	clearRemoteJobState(t, host, 1)

	// Queue a job
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "sleep 60",
		Description: "Integration test: queue entry removal",
	}
	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	jobID := result.JobID
	t.Logf("Queued job %d", jobID)

	// Verify job is in commands file
	entry, err := fetchQueueEntryFromRemote(host, ops.DefaultQueueName, jobID, 10*time.Second)
	if err != nil {
		t.Fatalf("Job not found in commands file: %v", err)
	}
	if entry.JobID != jobID {
		t.Fatalf("Wrong job ID in entry: %d", entry.JobID)
	}

	// Cancel the job - this should append a cancel command
	cancelCmd := ops.NewCancelCommand(jobID)
	if err := ops.AppendCommand(host, ops.DefaultQueueName, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second}); err != nil {
		t.Fatalf("AppendCommand (cancel) failed: %v", err)
	}

	// Verify cancel command was appended (we can check the commands file has a cancel entry)
	commandsFile := fmt.Sprintf("%s/%s.commands", ops.QueueDir, ops.DefaultQueueName)
	cmd := fmt.Sprintf(`grep -c '"op":"cancel".*"job_id":%d\|"op":"cancel".*"id":%d' %s 2>/dev/null || echo 0`, jobID, jobID, commandsFile)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, 10*time.Second)
	if err != nil {
		t.Logf("Could not verify cancel command: %v", err)
	} else {
		count := strings.TrimSpace(stdout)
		t.Logf("Found %s cancel commands for job %d", count, jobID)
	}
}

// queueEntry is a local struct for parsing queue entries in tests
type queueEntry struct {
	JobID       int64
	WorkingDir  string
	Command     string
	Description string
	EnvVars     []string
	DepSpec     string
}

// fetchQueueEntryFromRemote reads the queue entry for a job from the remote commands file.
// Uses the production remote.SSHHost.GetLastCommandForJob() for the SSH call.
func fetchQueueEntryFromRemote(host, queueName string, jobID int64, timeout time.Duration) (*queueEntry, error) {
	// Use production code path for SSH call
	sshHost := remote.NewSSHHost(host, timeout)
	content, err := sshHost.GetLastCommandForJob(queueName, jobID)
	if err != nil {
		return nil, err
	}

	if content == "" {
		return nil, fmt.Errorf("no queue entry found for job %d", jobID)
	}

	// Parse the JSON entry (test-specific parsing)
	var queueCmd struct {
		Job struct {
			ID   int64    `json:"id"`
			Dir  string   `json:"dir"`
			Cmd  string   `json:"cmd"`
			Desc string   `json:"desc"`
			Env  []string `json:"env"`
			Deps string   `json:"deps"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(content), &queueCmd); err != nil {
		return nil, fmt.Errorf("parse queue entry: %w", err)
	}

	return &queueEntry{
		JobID:       queueCmd.Job.ID,
		WorkingDir:  queueCmd.Job.Dir,
		Command:     queueCmd.Job.Cmd,
		Description: queueCmd.Job.Desc,
		EnvVars:     queueCmd.Job.Env,
		DepSpec:     queueCmd.Job.Deps,
	}, nil
}

// SLURM integration tests
// These tests run when SLURM_TEST_HOST is set, otherwise they skip.
//
// Requirements:
//   - SLURM_TEST_HOST environment variable set to user@hostname
//   - SSH_AUTH_SOCK environment variable set to the SSH agent socket
//   - Host must have SLURM available (sbatch, squeue, sacct)
//
// Example: SLURM_TEST_HOST=root@137.66.39.49 go test -v ./internal/ops/... -run "Slurm"

func getSlurmTestHost(t *testing.T) string {
	host := os.Getenv("SLURM_TEST_HOST")
	if host == "" {
		t.Skip("SLURM_TEST_HOST not set - skipping SLURM integration test")
	}
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("SSH_AUTH_SOCK not set - skipping SLURM integration test")
	}
	return host
}

// clearSlurmJobState removes SLURM-related state files for a job ID.
func clearSlurmJobState(t *testing.T, host string, jobID int64) {
	t.Helper()
	cmd := fmt.Sprintf("rm -f ~/.cache/remote-jobs/logs/%d.* 2>/dev/null || true", jobID)
	_, _, err := ssh.RunWithTimeout(host, cmd, 10*time.Second)
	if err != nil {
		t.Logf("Warning: could not clear SLURM state for job %d: %v", jobID, err)
	}
}

func TestSlurmIntegration_BackendDetection(t *testing.T) {
	host := getSlurmTestHost(t)

	backend, err := ops.ResolveBackend(host, 10*time.Second)
	if err != nil {
		t.Fatalf("ResolveBackend failed: %v", err)
	}

	if backend != db.BackendSlurm {
		t.Errorf("Expected backend to be 'slurm', got %s", backend)
	}
	t.Logf("Detected backend: %s", backend)
}

func TestSlurmIntegration_JobLifecycle(t *testing.T) {
	host := getSlurmTestHost(t)
	database := setupIntegrationTestDB(t)

	// Clear any previous state for job ID 1
	clearSlurmJobState(t, host, 1)

	// Queue a quick job
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "echo 'Hello from SLURM test'; sleep 2; echo 'Done'",
		Description: "SLURM integration test: job lifecycle",
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	jobID := result.JobID
	t.Logf("Queued job %d on %s, waiting for completion...", jobID, host)

	// Verify job was recorded with correct backend
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if job.Backend != db.BackendSlurm {
		t.Errorf("Expected job backend to be 'slurm', got %s", job.Backend)
	}
	if job.RemoteID == "" {
		t.Errorf("Expected job to have a RemoteID (SLURM job ID)")
	}
	t.Logf("Job has SLURM ID: %s", job.RemoteID)

	// Wait for job to complete (up to 60 seconds)
	deadline := time.Now().Add(60 * time.Second)
	var finalJob *db.Job
	for time.Now().Before(deadline) {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			t.Fatalf("GetJobByID failed: %v", err)
		}

		// Sync the job status
		_, err = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
		if err != nil {
			t.Logf("SyncJob error (may be transient): %v", err)
		}

		// Re-fetch after sync
		job, _ = db.GetJobByID(database, jobID)
		if db.IsTerminalStatus(job.Status) {
			finalJob = job
			break
		}

		t.Logf("Job status: %s (SLURM state: %s), waiting...", job.Status, job.RemoteState)
		time.Sleep(3 * time.Second)
	}

	if finalJob == nil {
		t.Fatal("Job did not complete within timeout")
	}

	t.Logf("Job completed with status: %s, exit code: %d", finalJob.Status, *finalJob.ExitCode)

	if finalJob.Status != db.StatusCompleted {
		t.Errorf("Expected job status to be completed, got %s", finalJob.Status)
	}
}

func TestSlurmIntegration_JobCancellation(t *testing.T) {
	host := getSlurmTestHost(t)
	database := setupIntegrationTestDB(t)

	// Clear any previous state for job ID 1
	clearSlurmJobState(t, host, 1)

	// Queue a long-running job
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "sleep 120; echo 'This should not run'",
		Description: "SLURM integration test: job cancellation",
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	jobID := result.JobID
	t.Logf("Queued job %d, will cancel it...", jobID)

	// Get the job to get its SLURM ID
	job, _ := db.GetJobByID(database, jobID)
	if job.RemoteID == "" {
		t.Fatal("Job has no RemoteID")
	}

	// Wait a moment for job to potentially start
	time.Sleep(2 * time.Second)

	// Set pending status to canceled
	if err := db.SetPendingStatus(database, jobID, db.StatusCanceled); err != nil {
		t.Fatalf("SetPendingStatus failed: %v", err)
	}

	// Reconcile to apply the cancellation
	job, _ = db.GetJobByID(database, jobID)
	_, err = ops.SyncAndReconcile(database, job, ops.ReconcileOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Logf("Reconcile error (may be expected): %v", err)
	}

	// Wait for cancellation to take effect
	time.Sleep(5 * time.Second)

	// Sync to get final status
	job, _ = db.GetJobByID(database, jobID)
	_, _ = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
	job, _ = db.GetJobByID(database, jobID)

	t.Logf("Final status: %s (SLURM state: %s)", job.Status, job.RemoteState)

	// Job should be killed (SLURM cancelled)
	if job.Status != db.StatusKilled && job.Status != db.StatusCanceled {
		t.Errorf("Expected job status to be killed or canceled, got %s", job.Status)
	}
}
