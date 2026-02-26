package runner_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/ssh"
)

// Integration tests for the Go queue runner.
// These tests run when SSH_TEST_HOST is set, targeting a fly.io test server.
//
// The tests deploy the Go runner binary, start it, submit jobs, and verify
// that the runner produces correct state files, log files, and status files.
//
// Requirements:
//   - SSH_TEST_HOST environment variable set to user@hostname
//   - SSH_AUTH_SOCK environment variable set
//   - Go cross-compilation available for linux/amd64
//
// Example: SSH_TEST_HOST=root@37.16.31.67 go test -v ./internal/runner/... -run "Integration" -timeout 120s

const (
	remoteQueueDir = "~/.cache/weft/queue"
	remoteLogDir   = "~/.cache/weft/logs"
	remoteBinPath  = "~/.cache/weft/bin/weft-agent"
	testQueueName  = "gotest"
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
		rm -f %s/%s.* %s/runner-%s.log 2>/dev/null
		rm -f %s/%s.commands %s/%s.state.json 2>/dev/null
		# Kill any existing test runner
		if [ -f %s/%s.runner.pid ]; then
			kill $(cat %s/%s.runner.pid) 2>/dev/null || true
			rm -f %s/%s.runner.pid
		fi
	`,
		remoteQueueDir, testQueueName, remoteQueueDir, testQueueName,
		remoteQueueDir, testQueueName, remoteQueueDir, testQueueName,
		remoteQueueDir, testQueueName,
		remoteQueueDir, testQueueName,
		remoteQueueDir, testQueueName,
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
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' '%s run-queue %s'", session, remoteBinPath, testQueueName)
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
	stopCmd := ops.NewStopCommand()
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
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' '%s run-queue %s'", session, remoteBinPath, testQueueName)
	sshRun(t, host, tmuxCmd)
	defer sshRunMayFail(t, host, fmt.Sprintf("tmux kill-session -t '%s' 2>/dev/null", session))

	// Wait for runner to start
	time.Sleep(3 * time.Second)

	// Submit a test job
	addCmd := ops.QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        ops.OpAdd,
		Job: &ops.CommandJob{
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
	stopCmd := ops.NewStopCommand()
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

	// Parse as ops.RunnerState (the type used by the sync code)
	var runnerState ops.RunnerState
	if err := json.Unmarshal([]byte(stateContent), &runnerState); err != nil {
		t.Fatalf("State file not parseable as ops.RunnerState: %v\nContent: %s", err, stateContent)
	}

	t.Logf("State file parsed successfully: cursor_line=%d, pending=%v", runnerState.CursorLine, runnerState.Pending)
}
