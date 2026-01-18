package remote

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Integration tests for SSHHost against a real SSH server.
// These tests run by default when SSH_TEST_HOST is set, otherwise they skip.
//
// Requirements:
//   - SSH_TEST_HOST environment variable set to user@hostname
//   - SSH_AUTH_SOCK environment variable set to the SSH agent socket
//
// Current fly.io test server:
//   - App: remote-jobs-ssh-test
//   - Dedicated IP: 37.16.31.67
//   - User: root
//
// On macOS with 1Password, find the agent socket:
//   ls -la /private/tmp/com.apple.launchd.*/Listeners
//
// To redeploy or update the fly.io test server:
//   1. cd /tmp/fly-ssh-test
//   2. Update Dockerfile if needed (ensure your public key is correct)
//   3. fly deploy --no-cache

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

func TestSSHHostIntegration_IsJobInQueue(t *testing.T) {
	host := getTestHost(t)
	sshHost := NewSSHHost(host, 10*time.Second)

	// Test with a non-existent job - should return false without error
	inQueue, err := sshHost.IsJobInQueue("default", 999999)
	if err != nil {
		t.Fatalf("IsJobInQueue failed: %v", err)
	}
	if inQueue {
		t.Errorf("Expected job 999999 to not be in queue")
	}
}

func TestSSHHostIntegration_IsJobCurrent(t *testing.T) {
	host := getTestHost(t)
	sshHost := NewSSHHost(host, 10*time.Second)

	// Test with a non-existent job - should return false without error
	isCurrent, err := sshHost.IsJobCurrent("default", 999999)
	if err != nil {
		t.Fatalf("IsJobCurrent failed: %v", err)
	}
	if isCurrent {
		t.Errorf("Expected job 999999 to not be current")
	}
}

func TestSSHHostIntegration_GetJobCompletion(t *testing.T) {
	host := getTestHost(t)
	sshHost := NewSSHHost(host, 10*time.Second)

	// Test with a non-existent job - should return nil without error
	info, err := sshHost.GetJobCompletion(999999)
	if err != nil {
		t.Fatalf("GetJobCompletion failed: %v", err)
	}
	if info != nil {
		t.Errorf("Expected nil completion info for non-existent job")
	}
}

func TestSSHHostIntegration_IsProcessRunning(t *testing.T) {
	host := getTestHost(t)
	sshHost := NewSSHHost(host, 10*time.Second)

	// Test with a non-existent job - should return false without error
	running, err := sshHost.IsProcessRunning(999999)
	if err != nil {
		t.Fatalf("IsProcessRunning failed: %v", err)
	}
	if running {
		t.Errorf("Expected process for job 999999 to not be running")
	}
}

func TestSSHHostIntegration_GetRecentlyModifiedJobIDs(t *testing.T) {
	host := getTestHost(t)
	sshHost := NewSSHHost(host, 10*time.Second)

	// Test getting recently modified job IDs - should return empty list
	since := time.Now().Add(-1 * time.Hour)
	jobIDs, err := sshHost.GetRecentlyModifiedJobIDs(since)
	if err != nil {
		t.Fatalf("GetRecentlyModifiedJobIDs failed: %v", err)
	}
	t.Logf("Found %d recently modified job IDs", len(jobIDs))
}

func TestSSHHostIntegration_TmuxSessionExists(t *testing.T) {
	host := getTestHost(t)
	sshHost := NewSSHHost(host, 10*time.Second)

	// Test with a non-existent session - should return false without error
	exists, err := sshHost.TmuxSessionExists("nonexistent-session-12345")
	if err != nil {
		t.Fatalf("TmuxSessionExists failed: %v", err)
	}
	if exists {
		t.Errorf("Expected tmux session to not exist")
	}
}

func TestSSHProberIntegration_AllProbes(t *testing.T) {
	host := getTestHost(t)
	prober := NewSSHProber(host, 10*time.Second)

	// Test all probes return definitive results (not unknown) for non-existent job
	jobID := int64(999999)
	queueName := "default"

	inQueue := prober.ProbeInQueue(queueName, jobID)
	if inQueue != ProbeFalse {
		t.Errorf("ProbeInQueue: expected ProbeFalse, got %v", inQueue)
	}

	current := prober.ProbeCurrent(queueName, jobID)
	if current != ProbeFalse {
		t.Errorf("ProbeCurrent: expected ProbeFalse, got %v", current)
	}

	completed, info := prober.ProbeCompleted(jobID)
	if completed != ProbeFalse {
		t.Errorf("ProbeCompleted: expected ProbeFalse, got %v", completed)
	}
	if info != nil {
		t.Errorf("ProbeCompleted: expected nil info, got %v", info)
	}

	running := prober.ProbeProcessRunning(jobID)
	if running != ProbeFalse {
		t.Errorf("ProbeProcessRunning: expected ProbeFalse, got %v", running)
	}
}

func TestSSHHostIntegration_AppendToQueue(t *testing.T) {
	host := getTestHost(t)
	sshHost := NewSSHHost(host, 10*time.Second)

	// Test appending a command to the queue - should succeed
	testJobID := int64(888888)
	entry := QueueEntry{
		JobID:       testJobID,
		WorkingDir:  "/tmp",
		Command:     "echo test",
		Description: "test job",
	}
	err := sshHost.AppendToQueue("default", entry)
	if err != nil {
		t.Fatalf("AppendToQueue failed: %v", err)
	}

	// Verify the command was appended to the commands file
	inCommands, err := sshHost.IsJobInCommandsFile("default", testJobID)
	if err != nil {
		t.Fatalf("IsJobInCommandsFile failed: %v", err)
	}
	if !inCommands {
		t.Error("Expected job to be in commands file after appending")
	}

	// Clean up by canceling the job
	err = sshHost.RemoveFromQueue("default", testJobID)
	if err != nil {
		t.Logf("Warning: failed to cancel test job: %v", err)
	}
}

func TestSSHHostIntegration_AppendToQueueWithArtifactEnvVars(t *testing.T) {
	host := getTestHost(t)
	sshHost := NewSSHHost(host, 10*time.Second)

	// Test that artifact env vars are passed through when provided
	testJobID := int64(888889)
	envVars := []string{
		"RJ_JOB_ID=888889",
		"RJ_ARTIFACT_MANIFEST=~/.cache/remote-jobs/artifacts/888889.json",
		"RJ_ARTIFACT_ROOT=.",
		"MY_CUSTOM_VAR=test_value",
	}

	entry := QueueEntry{
		JobID:       testJobID,
		WorkingDir:  "/tmp",
		Command:     "env | grep -E 'RJ_|MY_CUSTOM'",
		Description: "env test",
		EnvVars:     envVars,
	}
	err := sshHost.AppendToQueue("default", entry)
	if err != nil {
		t.Fatalf("AppendToQueue failed: %v", err)
	}

	// Verify the command was appended to the commands file with env vars
	content, err := sshHost.GetLastCommandForJob("default", testJobID)
	if err != nil {
		t.Fatalf("GetLastCommandForJob failed: %v", err)
	}
	if content == "" {
		t.Fatal("Expected to find command in commands file")
	}

	// Verify env vars are in the command
	if !strings.Contains(content, "RJ_JOB_ID=888889") {
		t.Error("Expected command to contain RJ_JOB_ID env var")
	}
	if !strings.Contains(content, "RJ_ARTIFACT_MANIFEST=") {
		t.Error("Expected command to contain RJ_ARTIFACT_MANIFEST env var")
	}
	if !strings.Contains(content, "MY_CUSTOM_VAR=test_value") {
		t.Error("Expected command to contain MY_CUSTOM_VAR env var")
	}

	// Clean up by canceling the job
	err = sshHost.RemoveFromQueue("default", testJobID)
	if err != nil {
		t.Logf("Warning: failed to cancel test job: %v", err)
	}
}
