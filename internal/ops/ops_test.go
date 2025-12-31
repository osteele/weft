package ops

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// commandCapture captures SSH commands for testing
type commandCapture struct {
	mu       sync.Mutex
	commands []string
}

func (c *commandCapture) add(cmd string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commands = append(c.commands, cmd)
}

func (c *commandCapture) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.commands...)
}

func (c *commandCapture) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commands = nil
}

// mockExecCommandSuccess returns a mock exec.Cmd that succeeds
func mockExecCommandSuccess(capture *commandCapture) func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		// Capture SSH commands for inspection
		// SSH args look like: ssh -o ConnectTimeout=10 -o BatchMode=yes host command
		// or for simple Run: ssh host command
		if name == "ssh" && len(args) > 0 {
			// The last argument is the command
			capture.add(args[len(args)-1])
		}
		// Return a command that succeeds
		return exec.Command("true")
	}
}

// mockExecCommandConnectionError returns a mock that fails with connection error
func mockExecCommandConnectionError() func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "echo 'ssh: connect to host testhost port 22: Connection refused' >&2; exit 255")
	}
}

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	// Create a temp file for the database
	tmpfile, err := os.CreateTemp("", "ops-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := tmpfile.Name()
	tmpfile.Close()

	// Use SetDBPath which handles mutex locking
	cleanup := db.SetDBPath(tmpPath)
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmpPath)
	})

	database, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.Close()
	})

	return database
}

// TestKillJobRemovesPendingStartOperations tests that killing a job
// removes any pending run/restart operations for that job.
func TestKillJobRemovesPendingStartOperations(t *testing.T) {
	database := setupTestDB(t)

	// Create a test job
	jobID, err := db.RecordJobStarting(database, "testhost", "/tmp", "echo test", "test job")
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	// Simulate a pending run operation (as if job was created but host was offline)
	err = db.AddDeferredOperation(database, "testhost", db.OpRunJob, jobID, "", `{}`)
	if err != nil {
		t.Fatalf("Failed to add run operation: %v", err)
	}

	// Verify the operation exists
	hasRun, _ := db.HasPendingOperation(database, jobID, db.OpRunJob)
	if !hasRun {
		t.Fatal("Expected pending run_job operation before kill")
	}

	// Kill the job (this will fail on remote but should still clean up locally)
	_, _ = KillJob(database, job, ExecuteOptions{})

	// The pending run operation should be removed
	hasRun, _ = db.HasPendingOperation(database, jobID, db.OpRunJob)
	if hasRun {
		t.Error("Expected run_job operation to be removed after kill")
	}

	// Job should be marked as dead
	job, _ = db.GetJobByID(database, jobID)
	if job.Status != db.StatusDead {
		t.Errorf("Expected job status to be dead, got %s", job.Status)
	}
}

// TestKillJobRemovesPendingRestartOperations tests that killing removes
// pending restart operations.
func TestKillJobRemovesPendingRestartOperations(t *testing.T) {
	database := setupTestDB(t)

	// Create a test job
	jobID, err := db.RecordJobStarting(database, "testhost", "/tmp", "echo test", "test job")
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	// Simulate a pending restart operation
	err = db.AddDeferredOperation(database, "testhost", db.OpRestartJob, jobID, "", `{}`)
	if err != nil {
		t.Fatalf("Failed to add restart operation: %v", err)
	}

	hasRestart, _ := db.HasPendingOperation(database, jobID, db.OpRestartJob)
	if !hasRestart {
		t.Fatal("Expected pending restart_job operation before kill")
	}

	// Kill the job
	_, _ = KillJob(database, job, ExecuteOptions{})

	// The pending restart operation should be removed
	hasRestart, _ = db.HasPendingOperation(database, jobID, db.OpRestartJob)
	if hasRestart {
		t.Error("Expected restart_job operation to be removed after kill")
	}
}

// TestKillJobPreservesOtherJobsOperations tests that killing a job doesn't
// remove operations for other jobs on a different host.
func TestKillJobPreservesOtherJobsOperations(t *testing.T) {
	database := setupTestDB(t)

	// Create two test jobs on DIFFERENT hosts
	// (operations for the same host get drained together)
	jobID1, _ := db.RecordJobStarting(database, "host1", "/tmp", "echo 1", "job1")
	jobID2, _ := db.RecordJobStarting(database, "host2", "/tmp", "echo 2", "job2")

	job1, _ := db.GetJobByID(database, jobID1)

	// Add operations for both jobs
	db.AddDeferredOperation(database, "host1", db.OpRunJob, jobID1, "", `{}`)
	db.AddDeferredOperation(database, "host2", db.OpRunJob, jobID2, "", `{}`)

	// Kill job1 (this drains operations for host1 only)
	_, _ = KillJob(database, job1, ExecuteOptions{})

	// Job1's operation should be removed (by DeletePendingOperationsForJob)
	hasRun1, _ := db.HasPendingOperation(database, jobID1, db.OpRunJob)
	if hasRun1 {
		t.Error("Expected job1's run_job to be removed")
	}

	// Job2's operation should still exist (different host)
	hasRun2, _ := db.HasPendingOperation(database, jobID2, db.OpRunJob)
	if !hasRun2 {
		t.Error("Expected job2's run_job to still exist")
	}
}

// TestExecuteAllDeferredOperations_KillJob tests that kill operations execute correctly for queue runner jobs
func TestExecuteAllDeferredOperations_KillJob(t *testing.T) {
	capture := &commandCapture{}
	cleanup := ssh.SetExecCommand(mockExecCommandSuccess(capture))
	defer cleanup()

	database := setupTestDB(t)

	// Create a queued job (with QueueName) - these use PID-based kill
	jobID, _ := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job", "default")

	// Add a kill operation
	_, err := db.AddDeferredOperationReturningID(database, "testhost", db.OpKillJob, jobID, "", "")
	if err != nil {
		t.Fatalf("Failed to add kill operation: %v", err)
	}

	// Execute deferred operations
	result, err := ExecuteAllDeferredOperations(database, "testhost", ExecuteOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ExecuteAllDeferredOperations failed: %v", err)
	}

	if result.Completed != 1 {
		t.Errorf("Expected 1 completed operation, got %d", result.Completed)
	}

	// Verify kill command was called (uses PID-based kill for queue runner jobs)
	commands := capture.get()
	foundKill := false
	for _, cmd := range commands {
		// Queue runner jobs use PID-based kill
		if strings.Contains(cmd, "kill") && strings.Contains(cmd, "pid") {
			foundKill = true
			break
		}
	}
	if !foundKill {
		t.Errorf("Expected PID-based kill command for queue runner job, got: %v", commands)
	}

	// Verify operation was removed from queue
	ops, _ := db.GetDeferredOperations(database, "testhost")
	if len(ops) != 0 {
		t.Errorf("Expected 0 pending operations, got %d", len(ops))
	}
}

// TestExecuteAllDeferredOperations_KillRegularJob tests that kill operations execute correctly for regular tmux jobs
func TestExecuteAllDeferredOperations_KillRegularJob(t *testing.T) {
	capture := &commandCapture{}
	cleanup := ssh.SetExecCommand(mockExecCommandSuccess(capture))
	defer cleanup()

	database := setupTestDB(t)

	// Create a regular job with a session name - these use tmux kill-session
	jobID, _ := db.RecordJobStarting(database, "testhost", "/tmp", "echo test", "test job")
	// Set session name to make it a regular tmux job (not a queue runner job)
	sessionName := fmt.Sprintf("rj-%d", jobID)
	database.Exec(`UPDATE jobs SET session_name = ? WHERE id = ?`, sessionName, jobID)

	// Add a kill operation
	_, err := db.AddDeferredOperationReturningID(database, "testhost", db.OpKillJob, jobID, "", "")
	if err != nil {
		t.Fatalf("Failed to add kill operation: %v", err)
	}

	// Execute deferred operations
	result, err := ExecuteAllDeferredOperations(database, "testhost", ExecuteOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ExecuteAllDeferredOperations failed: %v", err)
	}

	if result.Completed != 1 {
		t.Errorf("Expected 1 completed operation, got %d", result.Completed)
	}

	// Verify tmux kill-session command was called
	commands := capture.get()
	foundKill := false
	for _, cmd := range commands {
		// Regular jobs use tmux kill-session
		if strings.Contains(cmd, "tmux kill-session") {
			foundKill = true
			break
		}
	}
	if !foundKill {
		t.Errorf("Expected tmux kill-session command for regular job, got: %v", commands)
	}

	// Verify operation was removed from queue
	ops, _ := db.GetDeferredOperations(database, "testhost")
	if len(ops) != 0 {
		t.Errorf("Expected 0 pending operations, got %d", len(ops))
	}
}

// TestExecuteAllDeferredOperations_RemoveQueued tests remove_queued operations
func TestExecuteAllDeferredOperations_RemoveQueued(t *testing.T) {
	capture := &commandCapture{}
	cleanup := ssh.SetExecCommand(mockExecCommandSuccess(capture))
	defer cleanup()

	database := setupTestDB(t)

	// Create a queued job
	jobID, _ := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job", "myqueue")

	// Add a remove_queued operation
	_, err := db.AddDeferredOperationReturningID(database, "testhost", db.OpRemoveQueued, jobID, "myqueue", "")
	if err != nil {
		t.Fatalf("Failed to add operation: %v", err)
	}

	// Execute deferred operations
	result, err := ExecuteAllDeferredOperations(database, "testhost", ExecuteOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ExecuteAllDeferredOperations failed: %v", err)
	}

	if result.Completed != 1 {
		t.Errorf("Expected 1 completed operation, got %d", result.Completed)
	}

	// Verify sed command was called with correct queue file
	commands := capture.get()
	foundSed := false
	for _, cmd := range commands {
		if strings.Contains(cmd, "sed -i") && strings.Contains(cmd, "myqueue.queue") {
			foundSed = true
			break
		}
	}
	if !foundSed {
		t.Errorf("Expected sed command for queue file, got: %v", commands)
	}
}

// TestExecuteAllDeferredOperations_QueueAdd tests queue_job operations with env vars
func TestExecuteAllDeferredOperations_QueueAdd(t *testing.T) {
	capture := &commandCapture{}
	cleanup := ssh.SetExecCommand(mockExecCommandSuccess(capture))
	defer cleanup()

	database := setupTestDB(t)

	// Create a queued job
	jobID, _ := db.RecordQueued(database, "testhost", "/home/user/project", "python train.py", "training job", "gpu")

	// Add a queue_job operation with env vars
	payload := `{"working_dir":"/home/user/project","command":"python train.py","description":"training job","env_vars":["CUDA_VISIBLE_DEVICES=0","DEBUG=1"],"dep_spec":""}`
	_, err := db.AddDeferredOperationReturningID(database, "testhost", db.OpQueueJob, jobID, "gpu", payload)
	if err != nil {
		t.Fatalf("Failed to add operation: %v", err)
	}

	// Execute deferred operations
	result, err := ExecuteAllDeferredOperations(database, "testhost", ExecuteOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ExecuteAllDeferredOperations failed: %v", err)
	}

	if result.Completed != 1 {
		t.Errorf("Expected 1 completed operation, got %d", result.Completed)
	}

	// Verify command was called to append to queue file
	commands := capture.get()
	foundAppend := false
	for _, cmd := range commands {
		if strings.Contains(cmd, "gpu.queue") {
			foundAppend = true
			// The command is now base64 encoded, extract and decode it
			// Format: ... echo 'BASE64' | base64 -d >> ...
			if strings.Contains(cmd, "base64 -d") {
				// Extract base64 string between single quotes
				start := strings.Index(cmd, "echo '") + 6
				end := strings.Index(cmd[start:], "'") + start
				if start > 5 && end > start {
					b64 := cmd[start:end]
					decoded, err := base64.StdEncoding.DecodeString(b64)
					if err != nil {
						t.Errorf("Failed to decode base64: %v", err)
					} else if !strings.Contains(string(decoded), "python train.py") {
						t.Errorf("Decoded command should contain 'python train.py', got: %s", string(decoded))
					}
				}
			}
			break
		}
	}
	if !foundAppend {
		t.Errorf("Expected command to append to queue file, got: %v", commands)
	}
}

// TestExecuteAllDeferredOperations_StartQueued tests start_queued_job operations
func TestExecuteAllDeferredOperations_StartQueued(t *testing.T) {
	capture := &commandCapture{}
	cleanup := ssh.SetExecCommand(mockExecCommandSuccess(capture))
	defer cleanup()

	database := setupTestDB(t)

	// Create a queued job
	jobID, _ := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job", "default")

	// Add a start_queued_job operation
	_, err := db.AddDeferredOperationReturningID(database, "testhost", db.OpStartQueuedJob, jobID, "default", "")
	if err != nil {
		t.Fatalf("Failed to add operation: %v", err)
	}

	// Execute deferred operations
	result, err := ExecuteAllDeferredOperations(database, "testhost", ExecuteOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ExecuteAllDeferredOperations failed: %v", err)
	}

	if result.Completed != 1 {
		t.Errorf("Expected 1 completed operation, got %d", result.Completed)
	}

	// Verify start_now file was written to
	commands := capture.get()
	foundStartNow := false
	for _, cmd := range commands {
		if strings.Contains(cmd, ".start_now") {
			foundStartNow = true
			break
		}
	}
	if !foundStartNow {
		t.Errorf("Expected command to write to .start_now file, got: %v", commands)
	}
}

// TestExecuteAllDeferredOperations_MoveFromQueue tests move_from_queue operations
func TestExecuteAllDeferredOperations_MoveFromQueue(t *testing.T) {
	capture := &commandCapture{}
	cleanup := ssh.SetExecCommand(mockExecCommandSuccess(capture))
	defer cleanup()

	database := setupTestDB(t)

	// Create a job
	jobID, _ := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job", "default")

	// Add a move_from_queue operation (removing from old host's queue)
	_, err := db.AddDeferredOperationReturningID(database, "oldhost", db.OpMoveFromQueue, jobID, "default", "")
	if err != nil {
		t.Fatalf("Failed to add operation: %v", err)
	}

	// Execute deferred operations for oldhost
	result, err := ExecuteAllDeferredOperations(database, "oldhost", ExecuteOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ExecuteAllDeferredOperations failed: %v", err)
	}

	if result.Completed != 1 {
		t.Errorf("Expected 1 completed operation, got %d", result.Completed)
	}

	// Verify sed command was called to remove from queue
	commands := capture.get()
	foundSed := false
	for _, cmd := range commands {
		if strings.Contains(cmd, "sed -i") && strings.Contains(cmd, "default.queue") {
			foundSed = true
			break
		}
	}
	if !foundSed {
		t.Errorf("Expected sed command for queue removal, got: %v", commands)
	}
}

// TestExecuteAllDeferredOperations_ConnectionError tests that operations stay queued on connection error
func TestExecuteAllDeferredOperations_ConnectionError(t *testing.T) {
	cleanup := ssh.SetExecCommand(mockExecCommandConnectionError())
	defer cleanup()

	database := setupTestDB(t)

	// Create a job
	jobID, _ := db.RecordJobStarting(database, "testhost", "/tmp", "echo test", "test job")

	// Add a kill operation
	_, err := db.AddDeferredOperationReturningID(database, "testhost", db.OpKillJob, jobID, "", "")
	if err != nil {
		t.Fatalf("Failed to add kill operation: %v", err)
	}

	// Execute deferred operations - should fail with connection error
	result, err := ExecuteAllDeferredOperations(database, "testhost", ExecuteOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ExecuteAllDeferredOperations returned error: %v", err)
	}

	// On connection error, operations should NOT be completed
	if result.Completed != 0 {
		t.Errorf("Expected 0 completed operations on connection error, got %d", result.Completed)
	}

	// Operation should still be in the queue
	ops, _ := db.GetDeferredOperations(database, "testhost")
	if len(ops) != 1 {
		t.Errorf("Expected operation to remain queued, got %d operations", len(ops))
	}
}

// TestExecuteAllDeferredOperations_MultipleOps tests executing multiple operations in order
func TestExecuteAllDeferredOperations_MultipleOps(t *testing.T) {
	capture := &commandCapture{}
	cleanup := ssh.SetExecCommand(mockExecCommandSuccess(capture))
	defer cleanup()

	database := setupTestDB(t)

	// Create jobs
	jobID1, _ := db.RecordQueued(database, "testhost", "/tmp", "echo 1", "job1", "default")
	jobID2, _ := db.RecordQueued(database, "testhost", "/tmp", "echo 2", "job2", "default")

	// Add multiple operations
	db.AddDeferredOperationReturningID(database, "testhost", db.OpRemoveQueued, jobID1, "default", "")
	db.AddDeferredOperationReturningID(database, "testhost", db.OpRemoveQueued, jobID2, "default", "")

	// Execute deferred operations
	result, err := ExecuteAllDeferredOperations(database, "testhost", ExecuteOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ExecuteAllDeferredOperations failed: %v", err)
	}

	if result.Completed != 2 {
		t.Errorf("Expected 2 completed operations, got %d", result.Completed)
	}

	// All operations should be removed
	ops, _ := db.GetDeferredOperations(database, "testhost")
	if len(ops) != 0 {
		t.Errorf("Expected 0 pending operations, got %d", len(ops))
	}
}

func TestAppendQueueEntryFormatsQueueLine(t *testing.T) {
	capture := &commandCapture{}
	cleanup := ssh.SetExecCommand(mockExecCommandSuccess(capture))
	defer cleanup()

	entry := QueueEntry{
		JobID:       42,
		WorkingDir:  "",
		Command:     "python train.py",
		Description: "test job",
		EnvVars:     []string{"CUDA_VISIBLE_DEVICES=0", "DEBUG=1"},
		DepSpec:     "10,20:any",
	}

	err := AppendQueueEntry("testhost", "gpu", entry, AppendQueueEntryOptions{})
	if err != nil {
		t.Fatalf("AppendQueueEntry returned error: %v", err)
	}

	commands := capture.get()
	var appendCmd string
	for _, cmd := range commands {
		if strings.Contains(cmd, "remote-jobs/queue/gpu.queue") {
			appendCmd = cmd
			break
		}
	}
	if appendCmd == "" {
		t.Fatalf("expected AppendQueueEntry to invoke queue update, commands: %v", commands)
	}

	start := strings.Index(appendCmd, "echo '")
	if start == -1 {
		t.Fatalf("expected command to contain echo pipeline: %s", appendCmd)
	}
	start += len("echo '")
	end := strings.Index(appendCmd[start:], "' | base64 -d")
	if end == -1 {
		t.Fatalf("expected command to contain base64 decode: %s", appendCmd)
	}
	end += start

	b64 := appendCmd[start:end]
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("failed to decode base64 payload: %v", err)
	}

	line := string(decoded)
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("queue line must end with newline, got %q", line)
	}
	line = strings.TrimSuffix(line, "\n")

	parts := strings.Split(line, "\t")
	if len(parts) != 6 {
		t.Fatalf("expected 6 queue columns, got %d (%v)", len(parts), parts)
	}
	if parts[0] != "42" {
		t.Fatalf("expected job id 42, got %q", parts[0])
	}
	if parts[1] != "" {
		t.Fatalf("expected empty working dir placeholder, got %q", parts[1])
	}
	if parts[2] != entry.Command {
		t.Fatalf("expected command %q, got %q", entry.Command, parts[2])
	}
	if parts[3] != entry.Description {
		t.Fatalf("expected description %q, got %q", entry.Description, parts[3])
	}

	wantEnv := base64.StdEncoding.EncodeToString([]byte(strings.Join(entry.EnvVars, "\n")))
	if parts[4] != wantEnv {
		t.Fatalf("expected env payload %q, got %q", wantEnv, parts[4])
	}
	if parts[5] != entry.DepSpec {
		t.Fatalf("expected dependency spec %q, got %q", entry.DepSpec, parts[5])
	}
}
