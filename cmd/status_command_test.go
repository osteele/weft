package cmd

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
)

func TestJobStatusCmdHasStatusFlags(t *testing.T) {
	for _, name := range []string{"sync", "no-sync", "fast", "wait", "wait-timeout", "ssh-timeout"} {
		if flag := jobStatusCmd.Flags().Lookup(name); flag == nil {
			t.Fatalf("job status flag %q not found", name)
		}
	}
}

func TestRunStatusNoSyncSkipsRentalSync(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID := createRentalQueuedJob(t, database)

	restoreStatusFlags(t)
	statusNoSync = true

	originalSyncRentalJobsStatus := syncRentalJobsStatusFunc
	t.Cleanup(func() {
		syncRentalJobsStatusFunc = originalSyncRentalJobsStatus
	})

	called := false
	syncRentalJobsStatusFunc = func(_ *sql.DB, _ time.Duration) bool {
		called = true
		return true
	}

	captureStdout(t, func() {
		if err := runStatus(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})

	if called {
		t.Fatal("expected --no-sync to skip rental sync entirely")
	}
}

func TestRunStatusFullSyncUsesNormalCloudSyncTimeout(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID := createRentalQueuedJob(t, database)

	restoreStatusFlags(t)
	statusSync = true

	originalSyncRentalJobsStatus := syncRentalJobsStatusFunc
	t.Cleanup(func() {
		syncRentalJobsStatusFunc = originalSyncRentalJobsStatus
	})

	var got []time.Duration
	syncRentalJobsStatusFunc = func(_ *sql.DB, timeout time.Duration) bool {
		got = append(got, timeout)
		return true
	}

	captureStdout(t, func() {
		if err := runStatus(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})

	if len(got) != 1 {
		t.Fatalf("syncRentalJobsStatus called %d times, want 1", len(got))
	}
	if got[0] != NormalCloudSyncTimeout {
		t.Fatalf("cloud sync timeout = %v, want %v", got[0], NormalCloudSyncTimeout)
	}
}

func TestRunJobInfoFullSyncUsesNormalCloudSyncTimeout(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID := createRentalQueuedJob(t, database)

	restoreJobInfoFlags(t)
	jobInfoSync = true

	originalQuickSyncJobs := quickSyncJobsFunc
	t.Cleanup(func() {
		quickSyncJobsFunc = originalQuickSyncJobs
	})

	var gotSSHTimeout time.Duration
	var gotCloudTimeout time.Duration
	quickSyncJobsFunc = func(_ *sql.DB, jobs []*db.Job, sshTimeout, cloudTimeout time.Duration) []string {
		if len(jobs) != 1 {
			t.Fatalf("quickSyncJobs got %d jobs, want 1", len(jobs))
		}
		if jobs[0].ID != jobID {
			t.Fatalf("quickSyncJobs job ID = %d, want %d", jobs[0].ID, jobID)
		}
		gotSSHTimeout = sshTimeout
		gotCloudTimeout = cloudTimeout
		return nil
	}

	captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})

	if gotSSHTimeout != NormalSyncTimeout {
		t.Fatalf("job info SSH timeout = %v, want %v", gotSSHTimeout, NormalSyncTimeout)
	}
	if gotCloudTimeout != NormalCloudSyncTimeout {
		t.Fatalf("job info cloud timeout = %v, want %v", gotCloudTimeout, NormalCloudSyncTimeout)
	}
}

func TestRunStatusShowsBlockedReasonFromQueueState(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp", "echo hi", "blocked status", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreStatusFlags(t)
	statusNoSync = true

	reason := "gpu gate: no GPU matching class ampere+"
	cleanupSSH := ssh.SetRunner(func(_ string, _ string) (string, string, error) {
		return fmt.Sprintf("RUNNER:yes\nCURRENT:\nDEPTH:1\nBLOCKED:%d:%s\nSTOP:no\n", jobID, reason), "", nil
	})
	t.Cleanup(cleanupSSH)

	out := captureStdout(t, func() {
		if err := runStatus(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})
	if !strings.Contains(out, "Status:   blocked") {
		t.Fatalf("missing blocked status, got:\n%s", out)
	}
	if !strings.Contains(out, "Reason:   "+reason) {
		t.Fatalf("missing blocked reason, got:\n%s", out)
	}
}

func TestRunJobInfoShowsBlockedReasonFromQueueState(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp", "echo hi", "blocked info", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true

	reason := "gpu gate: no GPU matching class ampere+"
	cleanupSSH := ssh.SetRunner(func(_ string, _ string) (string, string, error) {
		return fmt.Sprintf("RUNNER:yes\nCURRENT:\nDEPTH:1\nBLOCKED:%d:%s\nSTOP:no\n", jobID, reason), "", nil
	})
	t.Cleanup(cleanupSSH)

	out := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})
	if !strings.Contains(out, "Status:      blocked") {
		t.Fatalf("missing blocked status, got:\n%s", out)
	}
	if !strings.Contains(out, "Reason:      "+reason) {
		t.Fatalf("missing blocked reason, got:\n%s", out)
	}
}

func createRentalQueuedJob(t *testing.T, database *sql.DB) int64 {
	t.Helper()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, db.LaunchHost(instanceID), "/tmp", "echo hi", "test rental", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	return jobID
}

func restoreStatusFlags(t *testing.T) {
	t.Helper()

	origSync := statusSync
	origNoSync := statusNoSync
	origFast := statusFast
	origWait := statusWait
	origWaitTimeout := statusWaitTimeout
	origSSHTimeout := statusSSHTimeout

	t.Cleanup(func() {
		statusSync = origSync
		statusNoSync = origNoSync
		statusFast = origFast
		statusWait = origWait
		statusWaitTimeout = origWaitTimeout
		statusSSHTimeout = origSSHTimeout
	})

	statusSync = false
	statusNoSync = false
	statusFast = false
	statusWait = false
	statusWaitTimeout = 0
	statusSSHTimeout = 0
}

func restoreJobInfoFlags(t *testing.T) {
	t.Helper()

	origSync := jobInfoSync
	origNoSync := jobInfoNoSync

	t.Cleanup(func() {
		jobInfoSync = origSync
		jobInfoNoSync = origNoSync
	})

	jobInfoSync = false
	jobInfoNoSync = false
}
