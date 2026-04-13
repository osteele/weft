package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/oplog"
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
	logOps = false
	logOpsJob = 0
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
