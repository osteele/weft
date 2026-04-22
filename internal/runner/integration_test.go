//go:build integration
// +build integration

package runner_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/ssh"
)

// Integration tests for the Go queue runner.
//
// The tests deploy the Go runner binary, start it, submit jobs, and verify
// that the runner produces correct state files, log files, and status files.
//
// Requirements:
//   - SSH_TEST_HOST environment variable set to user@hostname
//   - SSH_AUTH_SOCK environment variable set
//   - Go cross-compilation available for linux/amd64
//
// Example:
//   SSH_TEST_HOST=user@host go test -tags integration -v ./internal/runner -run "Integration" -timeout 120s

const (
	testHomeDir    = "/tmp/weft-runner-integration"
	remoteQueueDir = testHomeDir + "/.cache/weft/queue"
	remoteLogDir   = testHomeDir + "/.cache/weft/logs"
	remoteBinPath  = "~/.cache/weft/bin/weft-agent"
	testQueueName  = "default"
)

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

// cleanupTestQueue removes all test queue state from the remote host.
func cleanupTestQueue(t *testing.T, host string) {
	t.Helper()
	cmd := fmt.Sprintf(`
		# Kill any existing test runner
		if [ -f %s/%s.runner.pid ]; then
			kill $(cat %s/%s.runner.pid) 2>/dev/null || true
		fi
		rm -rf %s 2>/dev/null
	`,
		remoteQueueDir, testQueueName,
		remoteQueueDir, testQueueName,
		testHomeDir,
	)
	sshRun(t, host, cmd)
}

// sshRun is a test helper that runs a command via SSH and returns stdout.
func sshRun(t *testing.T, host, cmd string) string {
	t.Helper()
	stdout, stderr, err := ssh.RunWithTimeout(host, cmd, 30*time.Second)
	if err != nil {
		t.Logf("SSH stderr: %s", stderr)
		t.Fatalf("SSH command failed: %v\nCommand: %s", err, cmd)
	}
	return strings.TrimSpace(stdout)
}

// sshRunMayFail runs a command and returns stdout without failing on error.
func sshRunMayFail(t *testing.T, host, cmd string) string {
	t.Helper()
	stdout, _, _ := ssh.RunWithTimeout(host, cmd, 30*time.Second)
	return strings.TrimSpace(stdout)
}

// TestIntegration_GoRunnerAgentBinaryExists verifies the agent binary can be
// deployed to the test host. This is a prerequisite for all other Go runner tests.
func TestIntegration_GoRunnerAgentBinaryExists(t *testing.T) {
	host := getTestHost(t)

	// Check if binary exists
	result := sshRunMayFail(t, host, fmt.Sprintf("test -x %s && echo exists || echo missing", remoteBinPath))
	if result == "exists" {
		// Check version
		version := sshRunMayFail(t, host, fmt.Sprintf("%s --version", remoteBinPath))
		t.Logf("Agent binary exists on %s: %s", host, version)
	} else {
		t.Logf("Agent binary not deployed to %s yet. Run 'go run . sync %s' to deploy.", host, host)
		t.Skip("Agent binary not deployed - skipping Go runner integration tests")
	}
}

// TestIntegration_GoRunnerStartStop verifies the Go runner can start and stop cleanly.
func TestIntegration_GoRunnerStartStop(t *testing.T) {
	host := getTestHost(t)

	// Skip if binary not deployed
	result := sshRunMayFail(t, host, fmt.Sprintf("test -x %s && echo exists || echo missing", remoteBinPath))
	if result != "exists" {
		t.Skip("Agent binary not deployed")
	}

	cleanupTestQueue(t, host)

	// Ensure directories exist
	sshRun(t, host, fmt.Sprintf("mkdir -p %s %s", remoteQueueDir, remoteLogDir))

	// Start the runner in background via tmux
	session := fmt.Sprintf("rj-gotest-%s", testQueueName)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' 'HOME=%s %s run-queue'", session, testHomeDir, remoteBinPath)
	sshRun(t, host, tmuxCmd)

	// Wait for PID file to appear
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pid := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%s.runner.pid 2>/dev/null", remoteQueueDir, testQueueName))
		if pid != "" {
			t.Logf("Go runner started with PID %s", pid)
			break
		}
		time.Sleep(1 * time.Second)
	}

	pidStr := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%s.runner.pid 2>/dev/null", remoteQueueDir, testQueueName))
	if pidStr == "" {
		t.Fatal("Runner PID file not created within timeout")
	}

	// Send stop command
	stopCmd := opsqueue.NewStopCommand()
	data, _ := json.Marshal(stopCmd)
	appendCmd := fmt.Sprintf("printf '%%s\\n' '%s' >> %s/%s.commands", string(data), remoteQueueDir, testQueueName)
	sshRun(t, host, appendCmd)

	// Wait for runner to exit
	deadline = time.Now().Add(15 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		result := sshRunMayFail(t, host, fmt.Sprintf("tmux has-session -t '%s' 2>/dev/null && echo running || echo stopped", session))
		if result == "stopped" {
			stopped = true
			break
		}
		time.Sleep(1 * time.Second)
	}

	if !stopped {
		// Force cleanup
		sshRunMayFail(t, host, fmt.Sprintf("tmux kill-session -t '%s' 2>/dev/null", session))
		t.Fatal("Runner did not stop within timeout")
	}

	// Verify state file exists
	stateContent := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%s.state.json 2>/dev/null", remoteQueueDir, testQueueName))
	if stateContent == "" {
		t.Error("Expected state file to exist after clean shutdown")
	}

	t.Log("Go runner started and stopped cleanly")
}

// TestIntegration_GoRunnerJobExecution verifies the Go runner can execute a job
// and produce the correct output files (log, status, meta).
func TestIntegration_GoRunnerJobExecution(t *testing.T) {
	host := getTestHost(t)

	// Skip if binary not deployed
	result := sshRunMayFail(t, host, fmt.Sprintf("test -x %s && echo exists || echo missing", remoteBinPath))
	if result != "exists" {
		t.Skip("Agent binary not deployed")
	}

	cleanupTestQueue(t, host)

	// Clean up any previous test job files
	testJobID := int64(999001)
	sshRun(t, host, fmt.Sprintf("rm -f %s/%d.* %s/job-%d.json 2>/dev/null || true", remoteLogDir, testJobID, remoteQueueDir, testJobID))

	// Ensure directories
	sshRun(t, host, fmt.Sprintf("mkdir -p %s %s", remoteQueueDir, remoteLogDir))

	// Start the runner
	session := fmt.Sprintf("rj-gotest-exec-%s", testQueueName)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' 'HOME=%s %s run-queue'", session, testHomeDir, remoteBinPath)
	sshRun(t, host, tmuxCmd)
	defer sshRunMayFail(t, host, fmt.Sprintf("tmux kill-session -t '%s' 2>/dev/null", session))

	// Wait for runner to start
	time.Sleep(3 * time.Second)

	// Submit a test job
	addCmd := opsqueue.QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        opsqueue.OpAdd,
		Job: &opsqueue.CommandJob{
			ID:  testJobID,
			Dir: "/tmp",
			Cmd: "echo 'hello from go runner'; sleep 1; echo 'goodbye'",
		},
	}
	data, _ := json.Marshal(addCmd)
	escaped := strings.ReplaceAll(string(data), "'", `'\''`)
	appendCmd := fmt.Sprintf("printf '%%s\\n' '%s' >> %s/%s.commands", escaped, remoteQueueDir, testQueueName)
	sshRun(t, host, appendCmd)

	t.Logf("Submitted job %d, waiting for completion...", testJobID)

	// Wait for job to complete (check for status file)
	deadline := time.Now().Add(30 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		statusResult := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.status 2>/dev/null", remoteLogDir, testJobID))
		if statusResult != "" {
			t.Logf("Job completed with status: %s", statusResult)
			if strings.TrimSpace(statusResult) != "0" {
				t.Errorf("Expected exit code 0, got %s", statusResult)
			}
			completed = true
			break
		}
		time.Sleep(2 * time.Second)
	}

	if !completed {
		t.Fatal("Job did not complete within timeout")
	}

	// Verify log file
	logContent := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.log 2>/dev/null", remoteLogDir, testJobID))
	if logContent == "" {
		t.Error("Expected log file to exist")
	} else {
		if !strings.Contains(logContent, "hello from go runner") {
			t.Errorf("Log should contain job output, got:\n%s", logContent)
		}
		if !strings.Contains(logContent, "=== START") {
			t.Errorf("Log should contain header, got:\n%s", logContent)
		}
	}

	// Verify meta file
	metaContent := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.meta 2>/dev/null", remoteLogDir, testJobID))
	if metaContent == "" {
		t.Error("Expected meta file to exist")
	} else {
		if !strings.Contains(metaContent, fmt.Sprintf("job_id=%d", testJobID)) {
			t.Errorf("Meta should contain job_id, got:\n%s", metaContent)
		}
		if !strings.Contains(metaContent, "working_dir=/tmp") {
			t.Errorf("Meta should contain working_dir, got:\n%s", metaContent)
		}
	}

	// Verify state file shows job as finished
	stateContent := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%s.state.json 2>/dev/null", remoteQueueDir, testQueueName))
	if stateContent != "" {
		var state map[string]interface{}
		if err := json.Unmarshal([]byte(stateContent), &state); err != nil {
			t.Errorf("State file is not valid JSON: %v", err)
		} else {
			// Pending should be empty
			if pending, ok := state["pending"].([]interface{}); ok {
				if len(pending) > 0 {
					t.Errorf("Expected empty pending list, got %v", pending)
				}
			}
		}
	}

	// Send stop command to clean up
	stopCmd := opsqueue.NewStopCommand()
	stopData, _ := json.Marshal(stopCmd)
	sshRun(t, host, fmt.Sprintf("printf '%%s\\n' '%s' >> %s/%s.commands", string(stopData), remoteQueueDir, testQueueName))

	t.Log("Go runner job execution test passed")
}

// TestIntegration_GoRunnerStateCompatibility verifies that the Go runner
// writes state files that the existing sync code (BatchSyncQueueRunnerJobs)
// can read correctly.
func TestIntegration_GoRunnerStateCompatibility(t *testing.T) {
	host := getTestHost(t)

	// Skip if binary not deployed
	result := sshRunMayFail(t, host, fmt.Sprintf("test -x %s && echo exists || echo missing", remoteBinPath))
	if result != "exists" {
		t.Skip("Agent binary not deployed")
	}

	// Read the state file written by a previous test run
	stateContent := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%s.state.json 2>/dev/null", remoteQueueDir, testQueueName))
	if stateContent == "" {
		t.Skip("No state file from previous test - run TestIntegration_GoRunnerJobExecution first")
	}

	// Parse as opsqueue.RunnerState (the type used by the sync code)
	var runnerState opsqueue.RunnerState
	if err := json.Unmarshal([]byte(stateContent), &runnerState); err != nil {
		t.Fatalf("State file not parseable as opsqueue.RunnerState: %v\nContent: %s", err, stateContent)
	}

	t.Logf("State file parsed successfully: cursor_line=%d, pending=%v", runnerState.CursorLine, runnerState.Pending)
}

// skipIfNoAgent checks if the agent binary is deployed and skips the test if not.
func skipIfNoAgent(t *testing.T, host string) {
	t.Helper()
	result := sshRunMayFail(t, host, fmt.Sprintf("test -x %s && echo exists || echo missing", remoteBinPath))
	if result != "exists" {
		t.Skip("Agent binary not deployed")
	}
}

// startTestRunner starts a Go runner in a tmux session and returns a cleanup function.
func startTestRunner(t *testing.T, host, sessionSuffix string) func() {
	t.Helper()
	session := fmt.Sprintf("rj-gotest-%s-%s", sessionSuffix, testQueueName)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' 'HOME=%s %s run-queue'", session, testHomeDir, remoteBinPath)
	sshRun(t, host, tmuxCmd)
	time.Sleep(3 * time.Second)
	return func() {
		sshRunMayFail(t, host, fmt.Sprintf("tmux kill-session -t '%s' 2>/dev/null", session))
	}
}

// submitJob submits a job to the test queue and returns the command used.
func submitJob(t *testing.T, host string, jobID int64, cmd string, tags []string) {
	t.Helper()
	addCmd := opsqueue.QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        opsqueue.OpAdd,
		Job: &opsqueue.CommandJob{
			ID:   jobID,
			Dir:  "/tmp",
			Cmd:  cmd,
			Tags: tags,
		},
	}
	data, _ := json.Marshal(addCmd)
	escaped := strings.ReplaceAll(string(data), "'", `'\''`)
	appendCmd := fmt.Sprintf("printf '%%s\\n' '%s' >> %s/%s.commands", escaped, remoteQueueDir, testQueueName)
	sshRun(t, host, appendCmd)
}

// sendCommand sends a queue command (priority, cancel, stop) to the test queue.
func sendCommand(t *testing.T, host string, cmd opsqueue.QueueCommand) {
	t.Helper()
	data, _ := json.Marshal(cmd)
	escaped := strings.ReplaceAll(string(data), "'", `'\''`)
	appendCmd := fmt.Sprintf("printf '%%s\\n' '%s' >> %s/%s.commands", escaped, remoteQueueDir, testQueueName)
	sshRun(t, host, appendCmd)
}

// waitForJobStatus waits until the job's status file appears or timeout.
func waitForJobStatus(t *testing.T, host string, jobID int64, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.status 2>/dev/null", remoteLogDir, jobID))
		if result != "" {
			return strings.TrimSpace(result)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Job %d did not complete within %s", jobID, timeout)
	return ""
}

// cleanupJobFiles removes log/status/meta files for test job IDs.
func cleanupJobFiles(t *testing.T, host string, jobIDs ...int64) {
	t.Helper()
	for _, id := range jobIDs {
		sshRunMayFail(t, host, fmt.Sprintf("rm -f %s/%d.* 2>/dev/null", remoteLogDir, id))
	}
}

// TestIntegration_GoRunnerJobCancel verifies that canceling a queued job
// prevents it from running while the currently running job completes normally.
func TestIntegration_GoRunnerJobCancel(t *testing.T) {
	host := getTestHost(t)
	skipIfNoAgent(t, host)
	cleanupTestQueue(t, host)

	jobA := int64(999010)
	jobB := int64(999011)
	cleanupJobFiles(t, host, jobA, jobB)
	sshRun(t, host, fmt.Sprintf("mkdir -p %s %s", remoteQueueDir, remoteLogDir))

	cleanup := startTestRunner(t, host, "cancel")
	defer cleanup()

	// Submit job A (sleeps 8s) and job B (quick echo)
	submitJob(t, host, jobA, "echo 'job-a-start'; sleep 8; echo 'job-a-done'", nil)
	time.Sleep(3 * time.Second) // Let runner pick up job A

	submitJob(t, host, jobB, "echo 'job-b-should-not-run'", nil)
	time.Sleep(1 * time.Second)

	// Cancel job B before it starts
	sendCommand(t, host, opsqueue.NewCancelCommand(jobB))

	// Wait for job A to complete
	statusA := waitForJobStatus(t, host, jobA, 30*time.Second)
	if statusA != "0" {
		t.Errorf("Job A expected exit code 0, got %s", statusA)
	}

	// Wait a bit for runner to process any remaining queue
	time.Sleep(5 * time.Second)

	// Verify job B was never executed (no status file)
	statusB := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.status 2>/dev/null", remoteLogDir, jobB))
	if statusB != "" {
		t.Errorf("Job B should not have run (was canceled), but got status: %s", statusB)
	}

	// Verify job B has no log file
	logB := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.log 2>/dev/null", remoteLogDir, jobB))
	if logB != "" {
		t.Errorf("Job B should have no log file, but got:\n%s", logB)
	}

	sendCommand(t, host, opsqueue.NewStopCommand())
	t.Log("Go runner cancel test passed")
}

// TestIntegration_GoRunnerJobPriority verifies that moving a job to the front
// of the queue causes it to run before other pending jobs.
func TestIntegration_GoRunnerJobPriority(t *testing.T) {
	host := getTestHost(t)
	skipIfNoAgent(t, host)
	cleanupTestQueue(t, host)

	jobA := int64(999020)
	jobB := int64(999021)
	jobC := int64(999022)
	cleanupJobFiles(t, host, jobA, jobB, jobC)
	sshRun(t, host, fmt.Sprintf("mkdir -p %s %s", remoteQueueDir, remoteLogDir))

	// Submit all three jobs BEFORE starting the runner, so priority takes effect
	submitJob(t, host, jobA, "sleep 3; echo 'done-a'", nil)
	submitJob(t, host, jobB, "sleep 3; echo 'done-b'", nil)
	submitJob(t, host, jobC, "sleep 3; echo 'done-c'", nil)

	// Prioritize C before starting runner
	sendCommand(t, host, opsqueue.NewPriorityCommand(jobC))

	// Now start the runner
	cleanup := startTestRunner(t, host, "priority")
	defer cleanup()

	// Wait for C to complete first (it was prioritized)
	statusC := waitForJobStatus(t, host, jobC, 30*time.Second)
	if statusC != "0" {
		t.Errorf("Job C expected exit code 0, got %s", statusC)
	}

	// At this point, A and B should not be done yet (or just starting)
	// Check that C finished before A and B by looking at meta file timestamps
	metaC := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.meta 2>/dev/null", remoteLogDir, jobC))
	if !strings.Contains(metaC, fmt.Sprintf("job_id=%d", jobC)) {
		t.Errorf("Job C meta file missing or incorrect: %s", metaC)
	}

	// Wait for all to finish
	waitForJobStatus(t, host, jobA, 30*time.Second)
	waitForJobStatus(t, host, jobB, 30*time.Second)

	// Verify C started before A by comparing meta start_time values
	metaA := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.meta 2>/dev/null", remoteLogDir, jobA))
	startC := extractMetaField(metaC, "start_time")
	startA := extractMetaField(metaA, "start_time")
	if startC != "" && startA != "" && startC > startA {
		t.Errorf("Job C (priority) should have started before A: C=%s, A=%s", startC, startA)
	}

	sendCommand(t, host, opsqueue.NewStopCommand())
	t.Log("Go runner priority test passed")
}

// extractMetaField extracts a field value from meta file content (key=value format).
func extractMetaField(meta, key string) string {
	for _, line := range strings.Split(meta, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return ""
}

// TestIntegration_GoRunnerExclusiveTag verifies that a job tagged "exclusive"
// runs alone — no other job starts until it completes.
func TestIntegration_GoRunnerExclusiveTag(t *testing.T) {
	host := getTestHost(t)
	skipIfNoAgent(t, host)
	cleanupTestQueue(t, host)

	jobExcl := int64(999030)
	jobNext := int64(999031)
	cleanupJobFiles(t, host, jobExcl, jobNext)
	sshRun(t, host, fmt.Sprintf("mkdir -p %s %s", remoteQueueDir, remoteLogDir))

	cleanup := startTestRunner(t, host, "exclusive")
	defer cleanup()

	// Submit exclusive job (sleeps 5s) then a non-exclusive job
	submitJob(t, host, jobExcl, "echo 'excl-start'; sleep 5; echo 'excl-done'", []string{"exclusive"})
	time.Sleep(1 * time.Second)
	submitJob(t, host, jobNext, "echo 'next-done'", nil)

	// Wait for both to complete
	statusExcl := waitForJobStatus(t, host, jobExcl, 30*time.Second)
	if statusExcl != "0" {
		t.Errorf("Exclusive job expected exit code 0, got %s", statusExcl)
	}

	statusNext := waitForJobStatus(t, host, jobNext, 30*time.Second)
	if statusNext != "0" {
		t.Errorf("Next job expected exit code 0, got %s", statusNext)
	}

	// Verify exclusive job finished before next job started
	metaExcl := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.meta 2>/dev/null", remoteLogDir, jobExcl))
	metaNext := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.meta 2>/dev/null", remoteLogDir, jobNext))

	endExcl := extractMetaField(metaExcl, "end_time")
	startNext := extractMetaField(metaNext, "start_time")

	if endExcl != "" && startNext != "" && endExcl > startNext {
		t.Errorf("Exclusive job should have ended before next job started: excl_end=%s, next_start=%s", endExcl, startNext)
	}

	sendCommand(t, host, opsqueue.NewStopCommand())
	t.Log("Go runner exclusive tag test passed")
}

// TestIntegration_GoRunnerMultipleJobs verifies that the runner can execute
// multiple non-exclusive jobs sequentially and all complete successfully.
func TestIntegration_GoRunnerMultipleJobs(t *testing.T) {
	host := getTestHost(t)
	skipIfNoAgent(t, host)
	cleanupTestQueue(t, host)

	jobA := int64(999040)
	jobB := int64(999041)
	cleanupJobFiles(t, host, jobA, jobB)
	sshRun(t, host, fmt.Sprintf("mkdir -p %s %s", remoteQueueDir, remoteLogDir))

	cleanup := startTestRunner(t, host, "multi")
	defer cleanup()

	// Submit two quick jobs
	submitJob(t, host, jobA, "echo 'multi-a-output'", nil)
	submitJob(t, host, jobB, "echo 'multi-b-output'", nil)

	// Wait for both to complete
	statusA := waitForJobStatus(t, host, jobA, 30*time.Second)
	if statusA != "0" {
		t.Errorf("Job A expected exit code 0, got %s", statusA)
	}

	statusB := waitForJobStatus(t, host, jobB, 30*time.Second)
	if statusB != "0" {
		t.Errorf("Job B expected exit code 0, got %s", statusB)
	}

	// Verify both produced log output
	logA := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.log 2>/dev/null", remoteLogDir, jobA))
	if !strings.Contains(logA, "multi-a-output") {
		t.Errorf("Job A log should contain output, got:\n%s", logA)
	}

	logB := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%d.log 2>/dev/null", remoteLogDir, jobB))
	if !strings.Contains(logB, "multi-b-output") {
		t.Errorf("Job B log should contain output, got:\n%s", logB)
	}

	// Verify state file shows both as finished
	stateContent := sshRunMayFail(t, host, fmt.Sprintf("cat %s/%s.state.json 2>/dev/null", remoteQueueDir, testQueueName))
	if stateContent != "" {
		var state map[string]any
		if err := json.Unmarshal([]byte(stateContent), &state); err != nil {
			t.Errorf("State file is not valid JSON: %v", err)
		} else {
			if pending, ok := state["pending"].([]any); ok && len(pending) > 0 {
				t.Errorf("Expected empty pending list, got %v", pending)
			}
		}
	}

	sendCommand(t, host, opsqueue.NewStopCommand())
	t.Log("Go runner multiple jobs test passed")
}
