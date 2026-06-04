//go:build multihost
// +build multihost

package ops_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/ssh"
)

// Integration tests for multi-host artifact and failure collection.
//
// These tests verify that BatchSyncQueueRunnerJobs correctly collects job
// artifacts (logs, status, meta) and failure_reason files from both macOS
// and Linux hosts running the Go queue runner.
//
// Requirements:
//   - DARWIN_TEST_HOST environment variable set to user@hostname (e.g. studio)
//   - LINUX_TEST_HOST environment variable set to user@hostname (e.g. cool30)
//   - SSH_AUTH_SOCK environment variable set
//   - Agent binary deployed to each host
//
// Example:
//   export $(cat .env | grep -v '^#' | xargs)
//   go test -tags multihost -v ./internal/ops -run "Integration_MultiHost" -timeout 300s

const (
	multihostHomeDir   = "/tmp/weft-multihost-integration"
	multihostQueueDir  = multihostHomeDir + "/.cache/weft/queue"
	multihostLogDir    = multihostHomeDir + "/.cache/weft/logs"
	multihostBinPath   = "~/.cache/weft/bin/weft-agent"
	multihostQueueName = "default"
)

type multihostTestHost struct {
	envVar string
	name   string // human-readable name for sub-test
}

var multihostHosts = []multihostTestHost{
	{"DARWIN_TEST_HOST", "darwin"},
	{"LINUX_TEST_HOST", "linux"},
}

func multihostSSHRun(t *testing.T, host, cmd string) string {
	t.Helper()
	stdout, stderr, err := ssh.RunWithTimeout(host, cmd, 30*time.Second)
	if err != nil {
		t.Logf("SSH stderr: %s", stderr)
		t.Fatalf("SSH command failed on %s: %v\nCommand: %s", host, err, cmd)
	}
	return strings.TrimSpace(stdout)
}

func multihostSSHRunMayFail(t *testing.T, host, cmd string) string {
	t.Helper()
	stdout, _, _ := ssh.RunWithTimeout(host, cmd, 30*time.Second)
	return strings.TrimSpace(stdout)
}

func multihostCleanup(t *testing.T, host string, jobIDs []int64) {
	t.Helper()
	// Kill any existing test runner
	sshCmd := fmt.Sprintf(`
		if [ -f %s/%s.runner.pid ]; then
			kill $(cat %s/%s.runner.pid) 2>/dev/null || true
		fi
		rm -rf %s 2>/dev/null
	`,
		multihostQueueDir, multihostQueueName,
		multihostQueueDir, multihostQueueName,
		multihostHomeDir,
	)
	multihostSSHRunMayFail(t, host, sshCmd)

	// Clean up job files
	for _, id := range jobIDs {
		multihostSSHRunMayFail(t, host, fmt.Sprintf("rm -f %s/%d.* 2>/dev/null", multihostLogDir, id))
	}
}

func multihostStartRunner(t *testing.T, host string) string {
	t.Helper()
	session := fmt.Sprintf("rj-%s", multihostQueueName)

	// Kill any existing session
	multihostSSHRunMayFail(t, host, fmt.Sprintf("tmux kill-session -t '%s' 2>/dev/null", session))

	// Ensure directories
	multihostSSHRun(t, host, fmt.Sprintf("mkdir -p %s %s", multihostQueueDir, multihostLogDir))

	// Start runner
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' 'HOME=%s %s run-queue'", session, multihostHomeDir, multihostBinPath)
	multihostSSHRun(t, host, tmuxCmd)

	// Wait for PID file
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		pid := multihostSSHRunMayFail(t, host, fmt.Sprintf("cat %s/%s.runner.pid 2>/dev/null", multihostQueueDir, multihostQueueName))
		if pid != "" {
			t.Logf("Runner started with PID %s on %s", pid, host)
			return session
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("Runner PID file not created within timeout on %s", host)
	return session
}

func multihostSubmitJob(t *testing.T, host string, jobID int64, cmd string) {
	t.Helper()
	addCmd := opsqueue.QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        opsqueue.OpAdd,
		Job: &opsqueue.CommandJob{
			ID:  jobID,
			Dir: "/tmp",
			Cmd: cmd,
		},
	}
	data, err := json.Marshal(addCmd)
	if err != nil {
		t.Fatalf("marshal queue command: %v", err)
	}
	escaped := strings.ReplaceAll(string(data), "'", `'\''`)
	appendCmd := fmt.Sprintf("printf '%%s\\n' '%s' >> %s/%s.commands", escaped, multihostQueueDir, multihostQueueName)
	multihostSSHRun(t, host, appendCmd)
}

func multihostWaitForStatus(t *testing.T, host string, jobID int64, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := multihostSSHRunMayFail(t, host, fmt.Sprintf("cat %s/%d.status 2>/dev/null", multihostLogDir, jobID))
		if status != "" {
			return strings.TrimSpace(status)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Job %d did not complete within %v on %s", jobID, timeout, host)
	return ""
}

func resolveHostSpec(t *testing.T, host string) inventory.HostSpec {
	t.Helper()

	// Try embedded inventory first (match by hostname, ignoring user@ prefix)
	hostname := host
	if idx := strings.Index(host, "@"); idx >= 0 {
		hostname = host[idx+1:]
	}
	if spec := inventory.FindHost(hostname); spec != nil {
		return *spec
	}

	// Fall back to probing via SSH
	osName := multihostSSHRun(t, host, "uname -s")
	arch := multihostSSHRun(t, host, "uname -m")

	goOS := strings.ToLower(osName)
	goArch := arch
	switch arch {
	case "x86_64":
		goArch = "amd64"
	case "aarch64", "arm64":
		goArch = "arm64"
	}

	return inventory.HostSpec{
		Name: hostname,
		OS:   goOS,
		Arch: goArch,
	}
}

func TestIntegration_MultiHost_ArtifactCollection(t *testing.T) {
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("SSH_AUTH_SOCK not set")
	}

	for _, th := range multihostHosts {
		host := os.Getenv(th.envVar)
		if host == "" {
			t.Logf("%s not set, skipping %s tests", th.envVar, th.name)
			continue
		}

		t.Run(th.name, func(t *testing.T) {
			// Ensure agent binary is deployed
			spec := resolveHostSpec(t, host)
			deployed, err := agentdeploy.EnsureAgentUpToDate(host, spec)
			if err != nil {
				t.Fatalf("Failed to deploy agent to %s: %v", host, err)
			}
			if deployed {
				t.Logf("Deployed fresh agent binary to %s (os=%s arch=%s)", host, spec.OS, spec.Arch)
			}

			// Job IDs - use large IDs unlikely to conflict
			const (
				successJobID = int64(880001)
				oomJobID     = int64(880002)
				errorJobID   = int64(880003)
			)
			allJobIDs := []int64{successJobID, oomJobID, errorJobID}

			// Clean up before and after
			multihostCleanup(t, host, allJobIDs)
			t.Cleanup(func() {
				multihostSSHRunMayFail(t, host, fmt.Sprintf("tmux kill-session -t 'rj-%s' 2>/dev/null", multihostQueueName))
				multihostCleanup(t, host, allJobIDs)
			})

			// Start runner
			multihostStartRunner(t, host)

			// Submit jobs
			multihostSubmitJob(t, host, successJobID, "echo hello; exit 0")
			multihostSubmitJob(t, host, oomJobID, "echo oom-test; exit 137")
			multihostSubmitJob(t, host, errorJobID, "echo error-test; exit 1")

			t.Log("Submitted 3 jobs, waiting for completion...")

			// Wait for all jobs to complete
			for _, id := range allJobIDs {
				multihostWaitForStatus(t, host, id, 60*time.Second)
			}

			t.Log("All jobs completed, running batch sync...")

			// Set up test database and create job records
			database := db.SetupTestDB(t)

			for _, id := range allJobIDs {
				_, err := db.RecordQueuedWithGPU(database, host, "/tmp", fmt.Sprintf("test-job-%d", id), "", "")
				if err != nil {
					t.Fatalf("Failed to create job record for %d: %v", id, err)
				}
			}

			// The job IDs in the DB won't match the remote IDs, so we need to
			// create records with the correct IDs. Use raw SQL for this.
			for i, id := range allJobIDs {
				dbID := int64(i + 1) // SetupTestDB starts from 1
				if dbID != id {
					_, err := database.Exec("UPDATE jobs SET id = ? WHERE id = ?", id, dbID)
					if err != nil {
						t.Fatalf("Failed to update job ID %d -> %d: %v", dbID, id, err)
					}
				}
			}

			// Build job list for batch sync
			var jobs []*db.Job
			for _, id := range allJobIDs {
				job, err := db.GetJobByID(database, id)
				if err != nil {
					t.Fatalf("Failed to get job %d: %v", id, err)
				}
				jobs = append(jobs, job)
			}

			// Run batch sync (use longer timeout — the bash/jq script can be slow)
			updated, err := ops.BatchSyncQueueRunnerJobs(database, host, jobs, 60*time.Second)
			if err != nil {
				t.Fatalf("BatchSyncQueueRunnerJobs failed: %v", err)
			}
			t.Logf("Batch sync updated %d jobs", updated)

			if updated != len(allJobIDs) {
				t.Errorf("Expected %d jobs updated, got %d", len(allJobIDs), updated)
			}

			// Verify success job
			t.Run("success_job", func(t *testing.T) {
				job, err := db.GetJobByID(database, successJobID)
				if err != nil {
					t.Fatalf("GetJobByID: %v", err)
				}
				if job.ExitCode == nil {
					t.Fatal("Expected exit code to be set")
				}
				if *job.ExitCode != 0 {
					t.Errorf("Expected exit code 0, got %d", *job.ExitCode)
				}
				if job.FailureReason != "" {
					t.Errorf("Expected no failure_reason, got %q", job.FailureReason)
				}
				if !db.IsTerminalStatus(job.Status) {
					t.Errorf("Expected terminal status, got %q", job.Status)
				}

				// Verify log content on remote
				logContent := multihostSSHRunMayFail(t, host, fmt.Sprintf("cat %s/%d.log 2>/dev/null", multihostLogDir, successJobID))
				if !strings.Contains(logContent, "hello") {
					t.Errorf("Expected log to contain 'hello', got: %s", logContent)
				}
			})

			// Verify OOM job (exit 137)
			t.Run("oom_job", func(t *testing.T) {
				job, err := db.GetJobByID(database, oomJobID)
				if err != nil {
					t.Fatalf("GetJobByID: %v", err)
				}
				if job.ExitCode == nil {
					t.Fatal("Expected exit code to be set")
				}
				if *job.ExitCode != 137 {
					t.Errorf("Expected exit code 137, got %d", *job.ExitCode)
				}
				if !strings.Contains(job.FailureReason, "oom") {
					t.Errorf("Expected failure_reason to contain 'oom', got %q", job.FailureReason)
				}

				// Verify failure_reason file exists on remote
				frContent := multihostSSHRunMayFail(t, host, fmt.Sprintf("cat %s/%d.failure_reason 2>/dev/null", multihostLogDir, oomJobID))
				if !strings.Contains(frContent, "oom") {
					t.Errorf("Expected remote failure_reason file to contain 'oom', got %q", frContent)
				}
			})

			// Verify generic error job (exit 1)
			t.Run("error_job", func(t *testing.T) {
				job, err := db.GetJobByID(database, errorJobID)
				if err != nil {
					t.Fatalf("GetJobByID: %v", err)
				}
				if job.ExitCode == nil {
					t.Fatal("Expected exit code to be set")
				}
				if *job.ExitCode != 1 {
					t.Errorf("Expected exit code 1, got %d", *job.ExitCode)
				}
				if !strings.Contains(job.FailureReason, "error") {
					t.Errorf("Expected failure_reason to contain 'error', got %q", job.FailureReason)
				}

				// Verify failure_reason file exists on remote
				frContent := multihostSSHRunMayFail(t, host, fmt.Sprintf("cat %s/%d.failure_reason 2>/dev/null", multihostLogDir, errorJobID))
				if !strings.Contains(frContent, "error") {
					t.Errorf("Expected remote failure_reason file to contain 'error', got %q", frContent)
				}
			})
		})
	}
}
