package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestJobLogCmdRegistersSameFlagsAsLogCmd(t *testing.T) {
	logCmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if got := jobLogCmd.Flags().Lookup(flag.Name); got == nil {
			t.Errorf("job log missing flag %q", flag.Name)
		}
	})
}

func TestJobLogArgsAllowOpsModeWithoutJobID(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	logOps = true
	if err := jobLogCmd.Args(jobLogCmd, nil); err != nil {
		t.Fatalf("jobLogCmd.Args returned %v, want nil", err)
	}
}

func TestRunLogForJob_QueuedPlacedJobHasNoLogsYet(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "rental:17", "/tmp/p", "echo hi", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	cmd := &cobra.Command{Use: "log"}
	addLogFlags(cmd)
	out := captureStdout(t, func() {
		if err := runLogForJob(cmd, database, jobID); err != nil {
			t.Fatalf("runLogForJob: %v", err)
		}
	})
	if !strings.Contains(out, "has not started yet; no logs are available") {
		t.Fatalf("output = %q, want queued no-log message", out)
	}
	if !strings.Contains(out, "Placement:   assigned to wi17") {
		t.Fatalf("output = %q, want placement context", out)
	}
	if !strings.Contains(out, "Queue reason: waiting for assigned target to start the job") {
		t.Fatalf("output = %q, want queue reason", out)
	}
}

func TestContextualizeCloudLogFetchErrorRunningJob(t *testing.T) {
	job := &db.Job{ID: 4035, Status: db.StatusRunning}
	err := contextualizeCloudLogFetchError(job, errors.New("log not found in R2 for job wj4035 (the job may not have produced output, or the instance was terminated before log upload)"))
	if err == nil {
		t.Fatal("err = nil, want contextual error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "job status is running") {
		t.Fatalf("error = %q, want running status context", msg)
	}
	if !strings.Contains(msg, "--follow") {
		t.Fatalf("error = %q, want live follow hint", msg)
	}
	if strings.Contains(msg, "may not have produced output") || strings.Contains(msg, "terminated before log upload") {
		t.Fatalf("error = %q, should not include terminal-job guesses for running job", msg)
	}
}

func TestContextualizeCloudLogFetchErrorTerminalJobKeepsDiagnosis(t *testing.T) {
	original := errors.New("log not found in R2 for job wj4035 (the job may not have produced output, or the instance was terminated before log upload)")
	job := &db.Job{ID: 4035, Status: db.StatusCompleted}
	err := contextualizeCloudLogFetchError(job, original)
	if err != original {
		t.Fatalf("err = %v, want original terminal diagnosis", err)
	}
}

func TestReadOpsEntriesIncludesSyncedInstanceLogs(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	home := t.TempDir()
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}

	if err := oplog.Init(oplog.DefaultLogPath(), 0); err != nil {
		t.Fatalf("oplog.Init: %v", err)
	}
	oplog.Log(oplog.OpCLICommand, oplog.WithDetail("local"))
	if err := oplog.Close(); err != nil {
		t.Fatalf("oplog.Close: %v", err)
	}

	instanceID := int64(17)
	instancePath := oplog.SyncedInstanceLogPath(instanceID)
	if err := os.MkdirAll(filepath.Dir(instancePath), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	instanceEntries := []byte(`{"t":"2026-01-02T03:04:05Z","op":"agent.start","detail":"remote"}` + "\n")
	if err := os.WriteFile(instancePath, instanceEntries, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := readOpsEntries()
	if err != nil {
		t.Fatalf("readOpsEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("readOpsEntries returned %d entries, want 2", len(entries))
	}

	foundRemote := false
	for _, entry := range entries {
		if entry.Detail == "remote" {
			foundRemote = true
			if entry.Host != db.LaunchHost(instanceID) {
				t.Fatalf("remote entry host = %q, want %q", entry.Host, db.LaunchHost(instanceID))
			}
		}
	}
	if !foundRemote {
		t.Fatal("did not find synced instance ops log entry")
	}
}

func resetLogModeState() {
	logFollow = false
	logLines = 50
	logFrom = 0
	logTo = 0
	logGrep = ""
	logFull = false
	logTimeout = 0
	logSync = false
	logNoSync = false
	logAttempt = 0
	logOps = false
	logOpsJob = 0
	logOpsJobRaw = ""
	logOpsHost = ""
	logOpsOp = ""
	logOpsSince = ""
	logOpsErrors = false
	logEvents = false
	logEventsKind = ""
	logEventsLaunch = ""
	logEventsStats = false
}

func TestShouldUseCachedLogForJob_RequiresMatchingRunID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	runID := int64(12)
	job := &db.Job{ID: 77, LatestRunID: &runID}

	if err := logcache.WriteForRun(job.ID, runID, "ok"); err != nil {
		t.Fatalf("WriteForRun: %v", err)
	}
	if !shouldUseCachedLogForJob(job) {
		t.Fatal("expected cache to be used when run IDs match")
	}

	if err := logcache.WriteForRun(job.ID, runID+1, "stale"); err != nil {
		t.Fatalf("WriteForRun stale: %v", err)
	}
	if shouldUseCachedLogForJob(job) {
		t.Fatal("expected cache to be rejected when run IDs mismatch")
	}
}

func TestShouldUseCachedLogForJob_RejectsLegacyCacheForRunAwareJob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	runID := int64(34)
	job := &db.Job{ID: 78, LatestRunID: &runID}

	if err := logcache.Write(job.ID, "legacy"); err != nil {
		t.Fatalf("Write legacy cache: %v", err)
	}
	if shouldUseCachedLogForJob(job) {
		t.Fatal("expected legacy cache to be rejected for run-aware job")
	}
}

func TestRunLogForJob_UsesCachedLogWhenSSHReturnsEOF(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()
	t.Setenv("HOME", t.TempDir())

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "studio", "/tmp/project", "echo hi", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	exitCode := 0
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exitCode, time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	if err := logcache.WriteWithMeta(jobID, "cached log\n", false); err != nil {
		t.Fatalf("WriteWithMeta: %v", err)
	}

	cleanupSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		if host != "studio" {
			t.Fatalf("host = %q, want studio", host)
		}
		return "", "", errors.New("SSH connection to studio failed: EOF")
	})
	defer cleanupSSH()

	cmd := &cobra.Command{Use: "log"}
	addLogFlags(cmd)
	logFull = true
	logFrom = 1

	out := captureStdout(t, func() {
		if err := runLogForJob(cmd, database, jobID); err != nil {
			t.Fatalf("runLogForJob: %v", err)
		}
	})
	if !strings.Contains(out, "cached log") {
		t.Fatalf("output = %q, want cached log", out)
	}
}

func TestRunLogFromR2UsesOnlyLatestAttempt(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	first, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID first: %v", err)
	}
	if first.LatestRunID == nil {
		t.Fatal("first latest_run_id is nil")
	}
	if _, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusRunning); err != nil {
		t.Fatalf("CreateAttempt latest: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID latest: %v", err)
	}
	if job.LatestRunID == nil {
		t.Fatal("latest_run_id is nil")
	}

	orig := fetchAndDisplayLogFromR2Func
	defer func() { fetchAndDisplayLogFromR2Func = orig }()
	var gotRunIDs []int64
	fetchAndDisplayLogFromR2Func = func(_ *cobra.Command, _ *db.Job, runID int64) error {
		gotRunIDs = append(gotRunIDs, runID)
		return errors.New("latest attempt has no log")
	}

	err = runLogFromR2(&cobra.Command{Use: "log"}, database, job)
	if err == nil {
		t.Fatal("runLogFromR2 returned nil error, want latest-attempt fetch error")
	}
	if len(gotRunIDs) != 1 {
		t.Fatalf("fetch calls = %v, want exactly one call", gotRunIDs)
	}
	if gotRunIDs[0] != *job.LatestRunID {
		t.Fatalf("runID = %d, want latest_run_id %d", gotRunIDs[0], *job.LatestRunID)
	}
	if gotRunIDs[0] == *first.LatestRunID {
		t.Fatalf("runID = first attempt %d; want latest attempt %d", gotRunIDs[0], *job.LatestRunID)
	}
}

func TestRunLogForAttempt_RejectsUnknownAttempt(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp/p", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "host-alpha", nil, db.StatusRunning); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	handled, err := runLogForAttempt(nil, database, job, 99)
	if !handled {
		t.Fatal("runLogForAttempt(unknown #) returned handled=false, want true")
	}
	if err == nil {
		t.Fatal("runLogForAttempt(unknown #) returned nil error, want failure")
	}
	if !strings.Contains(err.Error(), "no attempt #99") {
		t.Fatalf("error = %q, want it to mention missing attempt", err)
	}
}

func TestAttemptsOnHostFiltersAndOrders(t *testing.T) {
	attempts := []db.JobAttempt{
		{AttemptNumber: 4, Host: "host-alpha"},
		{AttemptNumber: 3, Host: "host-beta"},
		{AttemptNumber: 2, Host: "host-alpha"},
		{AttemptNumber: 1, Host: "host-alpha"},
	}
	got := attemptsOnHost(attempts, "host-alpha")
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, want := range []int{1, 2, 4} {
		if got[i].AttemptNumber != want {
			t.Errorf("got[%d].AttemptNumber = %d, want %d", i, got[i].AttemptNumber, want)
		}
	}
}

func TestRunLogForAttempt_OnPremNotOnAnyHost(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp/p", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	// Add a second attempt with no host (pending placement) — that is the
	// attempt the reader will refuse to look up since there's no host to
	// fetch from.
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	handled, err := runLogForAttempt(nil, database, job, 2)
	if !handled {
		t.Fatal("runLogForAttempt returned handled=false, want true")
	}
	if err == nil {
		t.Fatal("runLogForAttempt returned nil error, want 'not on host' failure")
	}
	if !strings.Contains(err.Error(), "no placement recorded") {
		t.Fatalf("error = %q, want 'no placement recorded' explanation", err)
	}
}

func TestRunLogForAttempt_OnPremLatestFallsThrough(t *testing.T) {
	resetLogModeState()
	defer resetLogModeState()

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp/p", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	// Single attempt on host-alpha is the latest; runLogForAttempt should
	// signal handled=false so the caller falls through to the live-log path.
	handled, err := runLogForAttempt(nil, database, job, 1)
	if handled {
		t.Fatalf("runLogForAttempt returned handled=true (err=%v), want false (fall-through)", err)
	}
	if err != nil {
		t.Fatalf("runLogForAttempt returned err=%v on fall-through, want nil", err)
	}
}

func TestShouldUseCloudLogsTrueForAssignedLaunchJob(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if !shouldUseCloudLogs(database, job) {
		t.Fatal("shouldUseCloudLogs() = false, want true for launch-assigned job")
	}
}

func TestShouldUseCloudLogsTrueForUnplacedJobWithCloudHistory(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:            db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonJobFailure,
		Provider:          "vastai",
		GPUSpec:           "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	exitCode := 1
	endTime := time.Now().Unix()
	if err := db.CloseAttempt(database, jobID, db.StatusFailed, &exitCode, endTime); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	if err := db.CloseLaunchAttempt(database, jobID, db.AttemptOutcomeFailed); err != nil {
		t.Fatalf("CloseLaunchAttempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusQueued, jobID); err != nil {
		t.Fatalf("set requested_status queued: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Host != "" {
		t.Fatalf("job.Host = %q, want empty host for unplaced state", job.Host)
	}
	if !shouldUseCloudLogs(database, job) {
		t.Fatal("shouldUseCloudLogs() = false, want true for unplaced job with cloud attempts")
	}
}

// Regression: a job whose latest attempt ran on an inventory host must not
// route to the cloud log path just because an *earlier* attempt was on a
// rental instance. Symptom before the fix: `weft jobs log` produced
// "log not found in R2 for job ... (instance was terminated before log
// upload)" for jobs that had since been requeued onto cool30 / cool100.
func TestShouldUseCloudLogsFalseWhenLatestAttemptIsInventory(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	// Attempt #1: canceled on a rental instance (mirrors wj2301 attempt #1).
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	endTime := time.Now().Unix() - 60
	if err := db.CloseAttempt(database, jobID, db.StatusCanceled, nil, endTime); err != nil {
		t.Fatalf("CloseAttempt cloud: %v", err)
	}
	if err := db.CloseLaunchAttempt(database, jobID, db.AttemptOutcomeCancelled); err != nil {
		t.Fatalf("CloseLaunchAttempt: %v", err)
	}

	// Attempt #2: ran on inventory host cool30, exited 1 (mirrors wj2301 #2).
	if _, err := db.CreateAttempt(database, jobID, "cool30", nil, db.StatusRunning); err != nil {
		t.Fatalf("CreateAttempt cool30: %v", err)
	}
	exit := 1
	if err := db.CloseAttempt(database, jobID, db.StatusFailed, &exit, time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt cool30 close: %v", err)
	}
	// Job is now awaiting retry.
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusQueued, jobID); err != nil {
		t.Fatalf("set requested_status queued: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if shouldUseCloudLogs(database, job) {
		attempts, _ := db.ListAttempts(database, jobID)
		t.Fatalf("shouldUseCloudLogs() = true, want false; latest attempt should be inventory cool30. attempts=%+v", attempts)
	}
}
