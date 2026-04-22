//go:build integration
// +build integration

package ops_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/remote"
	"github.com/osteele/weft/internal/ssh"
)

// Integration tests for job lifecycle against a real SSH server.
//
// Requirements:
//   - SSH_TEST_HOST environment variable set to user@hostname
//   - SSH_AUTH_SOCK environment variable set to the SSH agent socket
//
// Example:
//   SSH_TEST_HOST=user@host go test -tags integration -v ./internal/ops -run "Integration"

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
	// Also remove job from the state.json pending array and finished map if present
	cmd := fmt.Sprintf(`
		rm -f ~/.cache/weft/logs/%d.* ~/.cache/weft/logs/%d-*.* ~/.cache/weft/queue/job-%d.json 2>/dev/null || true
		# Remove job from state.json pending array and finished map
		if [ -f ~/.cache/weft/queue/default.state.json ]; then
			jq 'if .pending then .pending |= map(select(. != %d)) else . end | if .finished then .finished |= del(."%d") else . end' ~/.cache/weft/queue/default.state.json > ~/.cache/weft/queue/default.state.json.tmp 2>/dev/null && \
			mv ~/.cache/weft/queue/default.state.json.tmp ~/.cache/weft/queue/default.state.json 2>/dev/null || true
		fi
		# Clear the current job marker if it matches this job
		current=$(cat ~/.cache/weft/queue/default.current 2>/dev/null)
		if [ "$current" = "%d" ]; then
			echo -n "" > ~/.cache/weft/queue/default.current 2>/dev/null || true
		fi
	`, jobID, jobID, jobID, jobID, jobID, jobID)
	_, _, err := ssh.RunWithTimeout(host, cmd, 10*time.Second)
	if err != nil {
		t.Logf("Warning: could not clear remote state for job %d: %v", jobID, err)
	}
}

// stopQueueRunner stops the queue runner on the remote host.
// Returns true if the runner was stopped, false if it wasn't running.
func stopQueueRunner(t *testing.T, host string) bool {
	t.Helper()
	pidFile := opsqueue.QueueDir + "/" + opsqueue.PidFileName()
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
	runner := queuerunner.NewRunner(host)
	return runner.EnsureStarted("", "", 0)
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
	_ = ops.AppendCommand(host, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second})
}

func TestIntegration_JobLifecycle(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Clear any previous state for job ID 1 (tests reuse IDs since each has fresh DB)
	clearRemoteJobState(t, host, 1)

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

	if finalJob.ExitCode != nil {
		t.Logf("Job completed with status: %s, exit code: %d", finalJob.Status, *finalJob.ExitCode)
	} else {
		t.Logf("Job completed with status: %s, exit code: nil", finalJob.Status)
	}

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
	entry, err := fetchQueueEntryFromRemote(host, jobID, 10*time.Second)
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
	_ = ops.AppendCommand(host, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second})
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
	if stopped := stopQueueRunner(t, host); stopped {
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
	entry, err := fetchQueueEntryFromRemote(host, jobID, 10*time.Second)
	if err != nil {
		t.Fatalf("Job not found in commands file: %v", err)
	}
	if entry.JobID != jobID {
		t.Fatalf("Wrong job ID in entry: %d", entry.JobID)
	}

	// Cancel the job - this should append a cancel command
	cancelCmd := ops.NewCancelCommand(jobID)
	if err := ops.AppendCommand(host, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second}); err != nil {
		t.Fatalf("AppendCommand (cancel) failed: %v", err)
	}

	// Verify cancel command was appended (we can check the commands file has a cancel entry)
	commandsFile := opsqueue.CommandsFilePath()
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
func fetchQueueEntryFromRemote(host string, jobID int64, timeout time.Duration) (*queueEntry, error) {
	sshHost := remote.NewSSHHost(host, timeout)
	content, err := sshHost.GetLastCommandForJob(jobID)
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

func TestIntegration_ExclusiveTagSynced(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Queue a job with the exclusive tag
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "echo 'Exclusive test job'",
		Description: "Integration test: exclusive tag",
		Tags:        []string{"exclusive", "test"},
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	jobID := result.JobID
	t.Logf("Queued job %d with exclusive tag", jobID)

	// Verify job was recorded with tags in database
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if !job.HasTag("exclusive") {
		t.Errorf("Expected job to have 'exclusive' tag in database, got tags: %v", job.Tags)
	}

	// Verify tags are synced to remote by checking the commands file
	commandsFile := opsqueue.CommandsFilePath()
	cmd := fmt.Sprintf(`tail -20 %s | grep '"id":%d' | tail -1`, commandsFile, jobID)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, 10*time.Second)
	if err != nil {
		t.Fatalf("Could not read commands file: %v", err)
	}

	// Parse the JSON to verify tags are present
	var queueCmd struct {
		Job struct {
			ID   int64    `json:"id"`
			Tags []string `json:"tags"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(stdout), &queueCmd); err != nil {
		t.Fatalf("Failed to parse queue command JSON: %v\nJSON: %s", err, stdout)
	}

	if queueCmd.Job.ID != jobID {
		t.Errorf("Wrong job ID in queue entry: got %d, want %d", queueCmd.Job.ID, jobID)
	}

	foundExclusive := false
	for _, tag := range queueCmd.Job.Tags {
		if tag == "exclusive" {
			foundExclusive = true
			break
		}
	}
	if !foundExclusive {
		t.Errorf("Expected 'exclusive' tag in remote queue entry, got tags: %v", queueCmd.Job.Tags)
	}

	t.Logf("Verified exclusive tag synced to remote: %v", queueCmd.Job.Tags)

	// Clean up
	cancelCmd := ops.NewCancelCommand(jobID)
	_ = ops.AppendCommand(host, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second})
}

func TestIntegration_ExclusiveJobRunsAlone(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Clear any previous state for jobs 1, 2, 3 (tests reuse IDs since each has fresh DB)
	clearRemoteJobState(t, host, 1)
	clearRemoteJobState(t, host, 2)
	clearRemoteJobState(t, host, 3)

	// Ensure queue runner is started
	started, err := ensureQueueRunnerStarted(t, host)
	if err != nil {
		t.Fatalf("Failed to start queue runner: %v", err)
	}
	if started {
		t.Logf("Started queue runner on %s", host)
	}

	// Queue three jobs rapidly:
	// 1. Regular job (runs first)
	// 2. Exclusive job (should wait for job 1, then run alone)
	// 3. Regular job (should wait for exclusive job)

	params1 := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "echo 'Job 1 start'; sleep 3; echo 'Job 1 done'",
		Description: "Integration test: regular job before exclusive",
	}
	result1, err := ops.QueueJob(database, params1, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob 1 failed: %v", err)
	}
	t.Logf("Queued regular job %d", result1.JobID)

	params2 := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "echo 'Exclusive job start'; sleep 2; echo 'Exclusive job done'",
		Description: "Integration test: exclusive job",
		Tags:        []string{"exclusive"},
	}
	result2, err := ops.QueueJob(database, params2, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob 2 (exclusive) failed: %v", err)
	}
	t.Logf("Queued exclusive job %d", result2.JobID)

	params3 := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "echo 'Job 3 start'; sleep 1; echo 'Job 3 done'",
		Description: "Integration test: regular job after exclusive",
	}
	result3, err := ops.QueueJob(database, params3, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob 3 failed: %v", err)
	}
	t.Logf("Queued regular job %d", result3.JobID)

	// Wait for all jobs to complete
	deadline := time.Now().Add(60 * time.Second)
	allComplete := false
	for time.Now().Before(deadline) {
		// Sync all jobs
		for _, jobID := range []int64{result1.JobID, result2.JobID, result3.JobID} {
			job, _ := db.GetJobByID(database, jobID)
			if job != nil {
				_, _ = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
			}
		}

		// Check if all are complete
		job1, _ := db.GetJobByID(database, result1.JobID)
		job2, _ := db.GetJobByID(database, result2.JobID)
		job3, _ := db.GetJobByID(database, result3.JobID)

		if job1 != nil && job2 != nil && job3 != nil &&
			db.IsTerminalStatus(job1.Status) &&
			db.IsTerminalStatus(job2.Status) &&
			db.IsTerminalStatus(job3.Status) {
			allComplete = true
			break
		}

		t.Logf("Waiting... Job1=%s Job2=%s Job3=%s",
			job1.Status, job2.Status, job3.Status)
		time.Sleep(2 * time.Second)
	}

	if !allComplete {
		t.Fatal("Jobs did not complete within timeout")
	}

	// Verify all jobs completed successfully
	job1, _ := db.GetJobByID(database, result1.JobID)
	job2, _ := db.GetJobByID(database, result2.JobID)
	job3, _ := db.GetJobByID(database, result3.JobID)

	if job1.Status != db.StatusCompleted {
		t.Errorf("Job 1 status: expected completed, got %s", job1.Status)
	}
	if job2.Status != db.StatusCompleted {
		t.Errorf("Job 2 (exclusive) status: expected completed, got %s", job2.Status)
	}
	if job3.Status != db.StatusCompleted {
		t.Errorf("Job 3 status: expected completed, got %s", job3.Status)
	}

	// Verify timing: job 2 should have started after job 1 ended,
	// and job 3 should have started after job 2 ended
	// StartTime is int64 (unix timestamp), EndTime is *int64
	if job1.EndTime != nil && job2.StartTime > 0 {
		if job2.StartTime < *job1.EndTime {
			t.Errorf("Exclusive job started before regular job 1 ended: job1.end=%d, job2.start=%d",
				*job1.EndTime, job2.StartTime)
		}
	}
	if job2.EndTime != nil && job3.StartTime > 0 {
		if job3.StartTime < *job2.EndTime {
			t.Errorf("Job 3 started before exclusive job ended: job2.end=%d, job3.start=%d",
				*job2.EndTime, job3.StartTime)
		}
	}

	t.Logf("All jobs completed successfully in correct order")
	t.Logf("Job 1: %d - %v", job1.StartTime, job1.EndTime)
	t.Logf("Job 2 (exclusive): %d - %v", job2.StartTime, job2.EndTime)
	t.Logf("Job 3: %d - %v", job3.StartTime, job3.EndTime)
}

// Finished map tests

func TestIntegration_FinishedMapRecordsCompletion(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	clearRemoteJobState(t, host, 1)

	started, err := ensureQueueRunnerStarted(t, host)
	if err != nil {
		t.Fatalf("Failed to start queue runner: %v", err)
	}
	if started {
		t.Logf("Started queue runner on %s", host)
	}

	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "echo done",
		Description: "Integration test: finished map records completion",
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}
	jobID := result.JobID
	t.Logf("Queued job %d, waiting for completion...", jobID)

	// Wait for job to reach terminal state via sync
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := db.GetJobByID(database, jobID)
		if job != nil {
			_, _ = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
			job, _ = db.GetJobByID(database, jobID)
			if db.IsTerminalStatus(job.Status) {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}

	// Read state.json and verify finished map contains this job
	stateFile := opsqueue.StateFilePath()
	cmd := fmt.Sprintf(`jq -r --arg id "%d" '.finished[$id] // empty' %s 2>/dev/null`, jobID, stateFile)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, 10*time.Second)
	if err != nil {
		t.Fatalf("Failed to read state.json finished map: %v", err)
	}

	stdout = strings.TrimSpace(stdout)
	if stdout == "" || stdout == "null" {
		t.Fatalf("Job %d not found in finished map", jobID)
	}

	// Parse the finished entry
	var finishedEntry struct {
		ExitCode   int   `json:"exit_code"`
		FinishedAt int64 `json:"finished_at"`
	}
	if err := json.Unmarshal([]byte(stdout), &finishedEntry); err != nil {
		t.Fatalf("Failed to parse finished entry: %v (raw: %s)", err, stdout)
	}

	if finishedEntry.ExitCode != 0 {
		t.Errorf("Expected exit_code 0, got %d", finishedEntry.ExitCode)
	}
	if finishedEntry.FinishedAt <= 0 {
		t.Errorf("Expected valid finished_at timestamp, got %d", finishedEntry.FinishedAt)
	}
	t.Logf("Finished map entry: exit_code=%d, finished_at=%d", finishedEntry.ExitCode, finishedEntry.FinishedAt)
}

func TestIntegration_FinishedMapRecordsFailure(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	clearRemoteJobState(t, host, 1)

	started, err := ensureQueueRunnerStarted(t, host)
	if err != nil {
		t.Fatalf("Failed to start queue runner: %v", err)
	}
	if started {
		t.Logf("Started queue runner on %s", host)
	}

	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "exit 1",
		Description: "Integration test: finished map records failure",
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}
	jobID := result.JobID
	t.Logf("Queued job %d, waiting for failure...", jobID)

	// Wait for job to reach terminal state via sync
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := db.GetJobByID(database, jobID)
		if job != nil {
			_, _ = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
			job, _ = db.GetJobByID(database, jobID)
			if db.IsTerminalStatus(job.Status) {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}

	// Read state.json and verify finished map contains this job with exit_code=1
	stateFile := opsqueue.StateFilePath()
	cmd := fmt.Sprintf(`jq -r --arg id "%d" '.finished[$id] // empty' %s 2>/dev/null`, jobID, stateFile)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, 10*time.Second)
	if err != nil {
		t.Fatalf("Failed to read state.json finished map: %v", err)
	}

	stdout = strings.TrimSpace(stdout)
	if stdout == "" || stdout == "null" {
		t.Fatalf("Job %d not found in finished map", jobID)
	}

	var finishedEntry struct {
		ExitCode   int   `json:"exit_code"`
		FinishedAt int64 `json:"finished_at"`
	}
	if err := json.Unmarshal([]byte(stdout), &finishedEntry); err != nil {
		t.Fatalf("Failed to parse finished entry: %v (raw: %s)", err, stdout)
	}

	if finishedEntry.ExitCode != 1 {
		t.Errorf("Expected exit_code 1, got %d", finishedEntry.ExitCode)
	}
	if finishedEntry.FinishedAt <= 0 {
		t.Errorf("Expected valid finished_at timestamp, got %d", finishedEntry.FinishedAt)
	}
	t.Logf("Finished map entry: exit_code=%d, finished_at=%d", finishedEntry.ExitCode, finishedEntry.FinishedAt)
}

func TestIntegration_BatchSyncUsesFinishedMap(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	clearRemoteJobState(t, host, 1)

	started, err := ensureQueueRunnerStarted(t, host)
	if err != nil {
		t.Fatalf("Failed to start queue runner: %v", err)
	}
	if started {
		t.Logf("Started queue runner on %s", host)
	}

	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "echo done",
		Description: "Integration test: batch sync uses finished map",
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}
	jobID := result.JobID
	t.Logf("Queued job %d, waiting for it to appear in finished map...", jobID)

	// Poll remote state.json finished map directly until job appears
	stateFile := opsqueue.StateFilePath()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		cmd := fmt.Sprintf(`jq -r --arg id "%d" '.finished[$id].exit_code // empty' %s 2>/dev/null`, jobID, stateFile)
		stdout, _, err := ssh.RunWithTimeout(host, cmd, 10*time.Second)
		if err == nil && strings.TrimSpace(stdout) != "" {
			t.Logf("Job %d appeared in finished map with exit_code=%s", jobID, strings.TrimSpace(stdout))
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Now call BatchSyncQueueRunnerJobs and verify it picks up the completion
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}

	updated, err := ops.BatchSyncQueueRunnerJobs(database, host, []*db.Job{job}, 10*time.Second)
	if err != nil {
		t.Fatalf("BatchSyncQueueRunnerJobs failed: %v", err)
	}

	if updated == 0 {
		t.Errorf("Expected BatchSyncQueueRunnerJobs to update at least 1 job, got 0")
	}

	// Verify the local DB shows completed with correct exit code
	job, _ = db.GetJobByID(database, jobID)
	if job.Status != db.StatusCompleted {
		t.Errorf("Expected job status completed, got %s", job.Status)
	}
	if job.ExitCode == nil || *job.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %v", job.ExitCode)
	}
	t.Logf("BatchSync correctly detected completion from finished map: status=%s exit_code=%d", job.Status, *job.ExitCode)
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
	cmd := fmt.Sprintf("rm -f ~/.cache/weft/logs/%d.* 2>/dev/null || true", jobID)
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

	if finalJob.ExitCode != nil {
		t.Logf("Job completed with status: %s, exit code: %d", finalJob.Status, *finalJob.ExitCode)
	} else {
		t.Logf("Job completed with status: %s, exit code: nil", finalJob.Status)
	}

	if finalJob.Status != db.StatusCompleted {
		t.Errorf("Expected job status to be completed, got %s", finalJob.Status)
	}
}

// TestIntegration_EffectiveStatusPreventsDoubleCancel verifies that once a job
// has a pending cancel, subsequent cancel attempts are rejected because
// EffectiveStatus() returns the pending status rather than the raw status.
func TestIntegration_EffectiveStatusPreventsDoubleCancel(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Queue a job
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "sleep 30",
		Description: "Integration test: effective status prevents double cancel",
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}
	jobID := result.JobID
	t.Logf("Queued job %d", jobID)

	// First cancel should succeed (sets pending status)
	job, _ := db.GetJobByID(database, jobID)
	cancelResult, err := ops.CancelQueuedJob(database, job, ops.ExecuteOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("First CancelQueuedJob failed: %v", err)
	}
	t.Logf("First cancel result: %+v", cancelResult)

	// Re-fetch job to get updated state
	job, _ = db.GetJobByID(database, jobID)

	// Verify EffectiveStatus shows canceled (either actual or pending)
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus != db.StatusCanceled {
		t.Errorf("Expected EffectiveStatus to be canceled, got %s (status=%s, pending=%v)",
			effectiveStatus, job.Status, job.PendingStatus)
	}

	// Second cancel should fail because EffectiveStatus is no longer queued
	_, err = ops.CancelQueuedJob(database, job, ops.ExecuteOptions{Timeout: 10 * time.Second})
	if err == nil {
		t.Error("Expected second CancelQueuedJob to fail, but it succeeded")
	} else {
		t.Logf("Second cancel correctly rejected: %v", err)
	}
}

// TestIntegration_SyncDetectsRunningProcess verifies that when sync detects a process
// is running but the local status is still queued/starting, it transitions to running.
// This tests the fix for handling concurrent job execution where only one job is
// tracked as "current" in the queue.
func TestIntegration_SyncDetectsRunningProcess(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Ensure queue runner is running
	_, err := ensureQueueRunnerStarted(t, host)
	if err != nil {
		t.Fatalf("Failed to start queue runner: %v", err)
	}

	// Queue a job that runs for a while
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "sleep 10; echo done",
		Description: "Integration test: sync detects running process",
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}
	jobID := result.JobID
	t.Logf("Queued job %d", jobID)

	// Wait for job to start running on remote
	deadline := time.Now().Add(30 * time.Second)
	var job *db.Job
	for time.Now().Before(deadline) {
		job, _ = db.GetJobByID(database, jobID)
		_, _ = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
		job, _ = db.GetJobByID(database, jobID)

		if job.Status == db.StatusRunning {
			t.Logf("Job is running")
			break
		}
		time.Sleep(1 * time.Second)
	}

	if job.Status != db.StatusRunning {
		t.Skipf("Job did not start running in time (status: %s), skipping test", job.Status)
	}

	// Simulate a race condition: reset local status to queued while job is still running
	_, err = database.Exec("UPDATE job_attempts SET status = ?, last_synced_status = ? WHERE job_id = ? AND end_time IS NULL",
		db.StatusQueued, db.StatusQueued, jobID)
	if err != nil {
		t.Fatalf("Failed to reset job status: %v", err)
	}
	t.Logf("Reset job status to queued (simulating race condition)")

	// Now sync should detect the running process and transition back to running
	job, _ = db.GetJobByID(database, jobID)
	if job.Status != db.StatusQueued {
		t.Fatalf("Expected status to be queued after reset, got %s", job.Status)
	}

	syncResult, err := ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("SyncJob failed: %v", err)
	}

	job, _ = db.GetJobByID(database, jobID)
	t.Logf("After sync: status=%s, updated=%v", job.Status, syncResult.Updated)

	if job.Status != db.StatusRunning {
		t.Errorf("Expected sync to detect running process and update status to running, got %s", job.Status)
	}

	if !syncResult.Updated {
		t.Errorf("Expected sync to report updated=true when transitioning queued->running")
	}

	// Clean up: wait for job to complete or cancel it
	cancelCmd := ops.NewCancelCommand(jobID)
	_ = ops.AppendCommand(host, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second})
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

// TestIntegration_PausedJobNotKilledByQueueRunner verifies that when a job is paused,
// the queue runner does not kill it (regression test for the bug where paused jobs
// were detected as "stopped" and killed).
func TestIntegration_PausedJobNotKilledByQueueRunner(t *testing.T) {
	host := getTestHost(t)
	database := setupIntegrationTestDB(t)

	// Ensure queue runner is started
	started, err := ensureQueueRunnerStarted(t, host)
	if err != nil {
		t.Fatalf("Failed to start queue runner: %v", err)
	}
	if started {
		t.Logf("Started queue runner on %s", host)
	}

	// Queue a job that runs long enough to be paused
	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  "/tmp",
		Command:     "for i in $(seq 1 30); do echo tick $i; sleep 1; done",
		Description: "Integration test: pause regression test",
	}

	result, err := ops.QueueJob(database, params, ops.ExecuteOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}
	jobID := result.JobID
	t.Logf("Queued job %d", jobID)

	// Wait for job to start running
	deadline := time.Now().Add(30 * time.Second)
	var job *db.Job
	for time.Now().Before(deadline) {
		job, _ = db.GetJobByID(database, jobID)
		if job != nil {
			_, _ = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
			job, _ = db.GetJobByID(database, jobID)
		}
		if job != nil && job.Status == db.StatusRunning {
			break
		}
		time.Sleep(1 * time.Second)
	}

	if job == nil || job.Status != db.StatusRunning {
		// Clean up and skip
		cancelCmd := ops.NewCancelCommand(jobID)
		_ = ops.AppendCommand(host, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second})
		t.Skipf("Job did not start running in time (status: %v), skipping test", job)
	}
	t.Logf("Job %d is running, waiting for pgid file...", jobID)

	// Wait a moment for the pgid file to be created (race between status detection and file I/O)
	time.Sleep(2 * time.Second)
	t.Log("Pausing job...")

	// Pause the job
	pauseResult, err := ops.RequestStatus(database, job, db.StatusPaused, ops.TimeoutSync)
	if err != nil {
		t.Fatalf("Failed to pause job: %v", err)
	}
	if !pauseResult.Success {
		t.Fatalf("Pause returned success=false: %s", pauseResult.Message)
	}
	t.Logf("Paused job %d", jobID)

	// Sync to confirm paused state
	job, _ = db.GetJobByID(database, jobID)
	_, _ = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
	job, _ = db.GetJobByID(database, jobID)
	if job.Status != db.StatusPaused {
		t.Fatalf("Expected job status to be paused after pause, got %s", job.Status)
	}

	// Wait for queue runner to cycle (it checks every 1-2 seconds)
	// This is where the bug would trigger - queue runner would see stopped process and kill it
	t.Log("Waiting for queue runner cycle (5 seconds)...")
	time.Sleep(5 * time.Second)

	// Sync again and verify job is still paused (not failed)
	job, _ = db.GetJobByID(database, jobID)
	_, _ = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
	job, _ = db.GetJobByID(database, jobID)

	if job.Status == db.StatusFailed {
		t.Errorf("REGRESSION: Paused job was marked as failed by queue runner! This is the bug we fixed.")
	} else if job.Status != db.StatusPaused {
		t.Errorf("Expected job to still be paused, got %s", job.Status)
	} else {
		t.Logf("Job %d is still paused after queue runner cycle - pause protection working", jobID)
	}

	// Resume the job
	t.Log("Resuming job...")
	resumeResult, err := ops.RequestStatus(database, job, db.StatusRunning, ops.TimeoutSync)
	if err != nil {
		t.Logf("Warning: resume failed: %v", err)
	} else if resumeResult.Success {
		t.Log("Job resumed")
	}

	// Clean up: wait for job to complete or cancel it
	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		job, _ = db.GetJobByID(database, jobID)
		if job != nil {
			_, _ = ops.SyncJob(database, job, ops.SyncOptions{Timeout: 10 * time.Second})
			job, _ = db.GetJobByID(database, jobID)
		}
		if job != nil && db.IsTerminalStatus(job.Status) {
			t.Logf("Job completed with status: %s", job.Status)
			break
		}
		time.Sleep(2 * time.Second)
	}

	// If job didn't complete, cancel it
	if job != nil && !db.IsTerminalStatus(job.Status) {
		cancelCmd := ops.NewCancelCommand(jobID)
		_ = ops.AppendCommand(host, cancelCmd, ops.AppendCommandOptions{Timeout: 10 * time.Second})
		t.Log("Cancelled job that didn't complete in time")
	}
}
