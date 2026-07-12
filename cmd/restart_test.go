package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/spf13/cobra"
)

func TestEnsureDispatchAfterRestart(t *testing.T) {
	origStatus := daemonStatusFunc
	origEnsure := ensureDaemonStartedFunc
	origWait := restartWait
	t.Cleanup(func() {
		daemonStatusFunc = origStatus
		ensureDaemonStartedFunc = origEnsure
		restartWait = origWait
	})

	ensureStub := func(called *bool) func(daemoncontrol.Paths, time.Duration) (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
		return func(daemoncontrol.Paths, time.Duration) (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
			*called = true
			return daemoncontrol.Status{Live: true}, daemoncontrol.EnsureNoop, nil
		}
	}

	t.Run("default live daemon returns fast without ensuring", func(t *testing.T) {
		restartWait = false
		daemonStatusFunc = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
			return daemoncontrol.Status{Live: true}, nil
		}
		ensured := false
		ensureDaemonStartedFunc = ensureStub(&ensured)
		var buf bytes.Buffer
		ensureDispatchAfterRestart(&buf)
		if ensured {
			t.Fatalf("did not expect a daemon ensure for a live daemon")
		}
		if buf.Len() != 0 {
			t.Fatalf("expected no output for a current live daemon, got %q", buf.String())
		}
	})

	t.Run("default live stale daemon hints without blocking", func(t *testing.T) {
		restartWait = false
		daemonStatusFunc = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
			return daemoncontrol.Status{Live: true, ActiveBinaryStale: true}, nil
		}
		ensured := false
		ensureDaemonStartedFunc = ensureStub(&ensured)
		var buf bytes.Buffer
		ensureDispatchAfterRestart(&buf)
		if ensured {
			t.Fatalf("did not expect a blocking daemon restart for a live stale daemon")
		}
		if !strings.Contains(buf.String(), "older binary") {
			t.Fatalf("expected a stale-daemon hint, got %q", buf.String())
		}
	})

	t.Run("default no live daemon starts one", func(t *testing.T) {
		restartWait = false
		daemonStatusFunc = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
			return daemoncontrol.Status{Live: false}, nil
		}
		ensured := false
		ensureDaemonStartedFunc = ensureStub(&ensured)
		var buf bytes.Buffer
		ensureDispatchAfterRestart(&buf)
		if !ensured {
			t.Fatalf("expected a daemon start when none is live")
		}
		if !strings.Contains(buf.String(), "Starting dispatch daemon") {
			t.Fatalf("expected a start message, got %q", buf.String())
		}
	})

	t.Run("wait ensures behind a visible line", func(t *testing.T) {
		restartWait = true
		statusProbed := false
		daemonStatusFunc = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
			statusProbed = true
			return daemoncontrol.Status{Live: true}, nil
		}
		ensured := false
		ensureDaemonStartedFunc = ensureStub(&ensured)
		var buf bytes.Buffer
		ensureDispatchAfterRestart(&buf)
		if !ensured {
			t.Fatalf("expected a daemon ensure with --wait")
		}
		if statusProbed {
			t.Fatalf("--wait should not consult the status probe")
		}
		if !strings.Contains(buf.String(), "Ensuring dispatch daemon is current") {
			t.Fatalf("expected a visible ensure line, got %q", buf.String())
		}
	})
}

func TestRestartCommandAliases(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"retry"})
	if err != nil {
		t.Fatalf("find top-level retry: %v", err)
	}
	if cmd != restartCmd {
		t.Fatalf("top-level retry resolved to %q, want restart command", cmd.Name())
	}

	cmd, _, err = jobCmd.Find([]string{"retry"})
	if err != nil {
		t.Fatalf("find job retry: %v", err)
	}
	if cmd != jobRestartCmd {
		t.Fatalf("job retry resolved to %q, want job restart command", cmd.Name())
	}
}

func TestRestartCloudJob_RefreshesProjectMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	cfg := "inputs = [\"hf:config-model\"]\n\n[outputs]\ndirs = [\"results/\"]\n"
	if err := os.WriteFile(filepath.Join(workDir, ".weft.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "cloud retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance id: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusFailed, jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if len(job.Inputs) != 1 || job.Inputs[0] != "hf:config-model" {
		t.Fatalf("job inputs = %v, want [hf:config-model]", job.Inputs)
	}
	if len(job.OutputDirs) != 1 || job.OutputDirs[0] != "results/" {
		t.Fatalf("job output dirs = %v, want [results/]", job.OutputDirs)
	}
	if job.LaunchID != nil {
		t.Fatalf("cloud instance id = %v, want nil", job.LaunchID)
	}
}

func TestRestartJob_RemovesProcessedTag(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "processed-retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	// Mark as cloud job so restart uses the ResetJobToUnplaced path (no SSH needed)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance id: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusFailed, jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if err := db.AddJobTag(database, jobID, db.ProcessedTag); err != nil {
		t.Fatalf("add processed tag: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.HasTag(db.ProcessedTag) {
		t.Fatalf("job still has processed tag after restart")
	}
	if job.EffectiveStatus() != db.StatusQueued {
		t.Fatalf("status after restart = %q, want queued", job.EffectiveStatus())
	}
}

func TestRestartQueuedJob_NoError(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
}

func TestRestartQueuedJob_NoChangesRefreshesProjectMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	cfg := "inputs = [\"hf:updated-model\"]\n\n[outputs]\ndirs = [\"results/\"]\n"
	if err := os.WriteFile(filepath.Join(workDir, ".weft.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(job.Inputs, []string{"hf:updated-model"}) {
		t.Fatalf("Inputs = %v, want [hf:updated-model]", job.Inputs)
	}
	if !slices.Equal(job.OutputDirs, []string{"results/"}) {
		t.Fatalf("OutputDirs = %v, want [results/]", job.OutputDirs)
	}
}

func TestResolveRestartTargetJobIDs_RequiresSelector(t *testing.T) {
	database := db.SetupTestDB(t)
	prev := restartUnplaced
	restartUnplaced = false
	t.Cleanup(func() { restartUnplaced = prev })

	_, err := resolveRestartTargetJobIDs(database, nil)
	if err == nil {
		t.Fatal("expected selector error")
	}
	if !strings.Contains(err.Error(), "use --unplaced") {
		t.Fatalf("error = %q, want mention of --unplaced", err.Error())
	}
}

func TestResolveRestartTargetJobIDs_UnplacedSelectsQueuedUnplacedJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	queuedID, err := db.RecordQueued(database, "", t.TempDir(), "python queued.py", "queued")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	placedID, err := db.RecordQueued(database, "cool30", t.TempDir(), "python placed.py", "placed")
	if err != nil {
		t.Fatalf("record placed: %v", err)
	}

	prev := restartUnplaced
	restartUnplaced = true
	t.Cleanup(func() { restartUnplaced = prev })

	jobIDs, err := resolveRestartTargetJobIDs(database, nil)
	if err != nil {
		t.Fatalf("resolveRestartTargetJobIDs: %v", err)
	}
	slices.Sort(jobIDs)
	want := []int64{queuedID}
	if !slices.Equal(jobIDs, want) {
		t.Fatalf("jobIDs = %v, want %v (placed=%d excluded)", jobIDs, want, placedID)
	}
}

func TestResolveRestartTargetJobIDs_UnplacedRejectsExplicitIDs(t *testing.T) {
	database := db.SetupTestDB(t)
	prev := restartUnplaced
	restartUnplaced = true
	t.Cleanup(func() { restartUnplaced = prev })

	_, err := resolveRestartTargetJobIDs(database, []string{"wj1"})
	if err == nil {
		t.Fatal("expected argument conflict error")
	}
	if !strings.Contains(err.Error(), "cannot combine job IDs with --unplaced") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestRestartQueuedJob_NoChanges_ResumesRunawayBreakerFromProjectScope(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	project := "retry-scope"
	if err := db.SetJobProject(database, jobID, project); err != nil {
		t.Fatalf("set project: %v", err)
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayTripped,
		CampaignID: 77,
		Detail:     "project=retry-scope; no-progress runaway: chain=3 orphaned=8 spend=$5.00 window=24h",
	}); err != nil {
		t.Fatalf("insert runaway tripped event: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}

	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
		Kind:       db.EventRelaunchRunawayResumed,
		CampaignID: 77,
	})
	if err != nil {
		t.Fatalf("list lifecycle events: %v", err)
	}
	found := false
	for _, event := range events {
		if strings.Contains(event.Detail, "project=retry-scope;") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected relaunch runaway resumed event for campaign 77 project %q", project)
	}
}

func TestRestartQueuedJob_NoChanges_ResumesRunawayBreakerFromAllScopeBlocked(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobProject(database, jobID, "role-encoding-injection"); err != nil {
		t.Fatalf("set project: %v", err)
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayBlocked,
		CampaignID: 225,
		Detail:     "project=<all>; runaway breaker tripped for scope; manual resume required",
	}); err != nil {
		t.Fatalf("insert runaway blocked event: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}

	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
		Kind:       db.EventRelaunchRunawayResumed,
		CampaignID: 225,
	})
	if err != nil {
		t.Fatalf("list lifecycle events: %v", err)
	}
	found := false
	for _, event := range events {
		if strings.Contains(event.Detail, "project=<all>;") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected relaunch runaway resumed event for <all> project scope")
	}
}

func TestRestartQueuedEndedLaunchJob_CreatesFreshAttemptAndRefreshesMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-mem = 24
# uv-args = ["--system"]
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry reset")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	attemptID, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusCanceled)
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET end_time = ?, status = ?, cloud_outcome = ?, pending_status = NULL, pending_at = NULL WHERE id = ?`,
		1_700_000_000, db.StatusCanceled, "canceled", attemptID); err != nil {
		t.Fatalf("mark attempt ended: %v", err)
	}
	oldMem := 8
	if err := db.SetJobGPUMemGB(database, jobID, &oldMem); err != nil {
		t.Fatalf("set initial gpu mem: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued-ended failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 26 {
		t.Fatalf("GPUMemGB = %v, want 26 (24 + headroom)", job.GPUMemGB)
	}
	if job.Command != "uv run --system train.py" {
		t.Fatalf("Command = %q, want %q", job.Command, "uv run --system train.py")
	}

	var latestAttemptNumber int
	var latestHost string
	var latestLaunchID any
	var latestStatus string
	var latestEndTime any
	var latestPendingStatus any
	err = database.QueryRow(`
		SELECT attempt_number, host, launch_id, status, end_time, pending_status
		FROM job_attempts
		WHERE job_id = ?
		ORDER BY attempt_number DESC
		LIMIT 1`, jobID).Scan(&latestAttemptNumber, &latestHost, &latestLaunchID, &latestStatus, &latestEndTime, &latestPendingStatus)
	if err != nil {
		t.Fatalf("query latest attempt: %v", err)
	}
	if latestAttemptNumber != 2 {
		t.Fatalf("latest attempt number = %d, want 2", latestAttemptNumber)
	}
	if latestHost != "" {
		t.Fatalf("latest host = %q, want empty for unplaced retry", latestHost)
	}
	if latestLaunchID != nil {
		t.Fatalf("latest launch_id = %v, want nil", latestLaunchID)
	}
	if latestStatus != db.StatusQueued {
		t.Fatalf("latest status = %q, want queued", latestStatus)
	}
	if latestEndTime != nil {
		t.Fatalf("latest end_time = %v, want nil", latestEndTime)
	}
	if latestPendingStatus != db.StatusQueued {
		t.Fatalf("latest pending_status = %v, want queued", latestPendingStatus)
	}

	var priorOutcome string
	if err := database.QueryRow(`SELECT COALESCE(cloud_outcome, '') FROM job_attempts WHERE job_id = ? AND attempt_number = 1`, jobID).
		Scan(&priorOutcome); err != nil {
		t.Fatalf("query prior cloud_outcome: %v", err)
	}
	if priorOutcome != db.AttemptOutcomeSuperseded {
		t.Fatalf("prior cloud_outcome = %q, want %q", priorOutcome, db.AttemptOutcomeSuperseded)
	}
}

func TestRestartQueuedWithCloudHistory_CreatesFreshAttempt(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry reset")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	// Historical cloud attempt (not superseded yet): this should trigger fresh retry behavior.
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	attempt1, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusCanceled)
	if err != nil {
		t.Fatalf("create cloud attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = ?, end_time = ?, status = ?, cloud_outcome = ? WHERE id = ?`,
		1_699_999_000, 1_700_000_000, db.StatusCanceled, db.AttemptOutcomeOrphaned, attempt1); err != nil {
		t.Fatalf("close cloud attempt: %v", err)
	}

	// Current queued open attempt (typical state after a previous retry/no-op path).
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("create open queued attempt: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued-with-cloud-history failed: %v", err)
	}

	var latestAttemptNumber int
	var latestStatus string
	var latestEndTime any
	err = database.QueryRow(`
		SELECT attempt_number, status, end_time
		FROM job_attempts
		WHERE job_id = ?
		ORDER BY attempt_number DESC
		LIMIT 1`, jobID).Scan(&latestAttemptNumber, &latestStatus, &latestEndTime)
	if err != nil {
		t.Fatalf("query latest attempt: %v", err)
	}
	if latestAttemptNumber != 3 {
		t.Fatalf("latest attempt number = %d, want 3", latestAttemptNumber)
	}
	if latestStatus != db.StatusQueued {
		t.Fatalf("latest status = %q, want queued", latestStatus)
	}
	if latestEndTime != nil {
		t.Fatalf("latest end_time = %v, want nil", latestEndTime)
	}

	var priorOutcome string
	if err := database.QueryRow(`SELECT COALESCE(cloud_outcome, '') FROM job_attempts WHERE job_id = ? AND attempt_number = 1`, jobID).
		Scan(&priorOutcome); err != nil {
		t.Fatalf("query prior cloud_outcome: %v", err)
	}
	if priorOutcome != db.AttemptOutcomeSuperseded {
		t.Fatalf("prior cloud_outcome = %q, want %q", priorOutcome, db.AttemptOutcomeSuperseded)
	}
}

func TestRestartQueuedWithCloudHistoryRollsBackMetadataOnFreshAttemptFailure(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry reset")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	initialReasons := []string{"planner: previous blocker"}
	if err := db.SetJobPlacementReasons(database, jobID, initialReasons); err != nil {
		t.Fatalf("set placement reasons: %v", err)
	}

	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	attempt1, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusCanceled)
	if err != nil {
		t.Fatalf("create cloud attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = ?, end_time = ?, status = ?, cloud_outcome = ? WHERE id = ?`,
		1_699_999_000, 1_700_000_000, db.StatusCanceled, db.AttemptOutcomeOrphaned, attempt1); err != nil {
		t.Fatalf("close cloud attempt: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("create open queued attempt: %v", err)
	}
	if _, err := database.Exec(`
		CREATE TRIGGER fail_retry_attempt
		BEFORE INSERT ON job_attempts
		WHEN NEW.job_id = ` + fmt.Sprint(jobID) + ` AND NEW.attempt_number > 2
		BEGIN
			SELECT RAISE(FAIL, 'forced retry insert failure');
		END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	err = restartJob(database, jobID, restartOverrides{GPUClass: "a100", HasAny: true})
	if err == nil {
		t.Fatal("restartJob succeeded, want forced trigger failure")
	}
	if !strings.Contains(err.Error(), "forced retry insert failure") {
		t.Fatalf("error = %v, want forced trigger failure", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUClass != "" {
		t.Fatalf("GPUClass = %q, want rollback to empty", job.GPUClass)
	}
	if job.CLIResourceOverrides != nil {
		t.Fatalf("CLIResourceOverrides = %+v, want nil after rollback", job.CLIResourceOverrides)
	}
	if !slices.Equal(job.PlacementReasons, initialReasons) {
		t.Fatalf("PlacementReasons = %v, want %v", job.PlacementReasons, initialReasons)
	}

	var attempts int
	if err := database.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id = ?`, jobID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempt count = %d, want original two attempts", attempts)
	}
}

func TestRestartQueuedExplicitHostWithStaleRentalTagKeepsHost(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "studio", t.TempDir(), "python train.py", "queued retry reset")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.AddJobTag(database, jobID, db.TagRental); err != nil {
		t.Fatalf("add stale rental tag: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusCanceled); err != nil {
		t.Fatalf("create historical cloud attempt: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "studio", nil, db.StatusQueued); err != nil {
		t.Fatalf("restore queued host attempt: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.TargetKind() != db.JobTargetInventoryHost {
		t.Fatalf("target kind = %q, want %q", job.TargetKind(), db.JobTargetInventoryHost)
	}
	if job.Host != "studio" {
		t.Fatalf("host = %q, want studio", job.Host)
	}
	if job.LaunchID != nil {
		t.Fatalf("launch_id = %v, want nil", job.LaunchID)
	}
	if job.PendingStatus == nil || *job.PendingStatus != db.StatusQueued {
		t.Fatalf("pending_status = %v, want queued", job.PendingStatus)
	}
}

func TestRestartLiveRentalJobKeepsLaunchTarget(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "live rental retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("set launch: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusFailed, int64(1000), db.AttemptOutcomeFailed, jobID,
	); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.TargetKind() != db.JobTargetRentalInstance {
		t.Fatalf("target kind = %q, want %q", job.TargetKind(), db.JobTargetRentalInstance)
	}
	if job.LaunchID == nil || *job.LaunchID != launchID {
		t.Fatalf("launch_id = %v, want %d", job.LaunchID, launchID)
	}
	if job.PendingStatus == nil || *job.PendingStatus != db.StatusQueued {
		t.Fatalf("pending_status = %v, want queued", job.PendingStatus)
	}
}

func TestRestartLiveRentalJobWithStoredPlacementHostKeepsHost(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "studio", t.TempDir(), "python train.py", "live rental retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("set launch: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusFailed, int64(1000), db.AttemptOutcomeFailed, jobID,
	); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.TargetKind() != db.JobTargetInventoryHost {
		t.Fatalf("target kind = %q, want %q", job.TargetKind(), db.JobTargetInventoryHost)
	}
	if job.Host != "studio" {
		t.Fatalf("host = %q, want studio", job.Host)
	}
	if job.LaunchID != nil {
		t.Fatalf("launch_id = %v, want nil", job.LaunchID)
	}
	if job.PendingStatus == nil || *job.PendingStatus != db.StatusQueued {
		t.Fatalf("pending_status = %v, want queued", job.PendingStatus)
	}
}

func TestRestartJobRejectsDeterministicPinnedHostGateMismatch(t *testing.T) {
	restore := inventory.SetHosts([]inventory.HostSpec{
		{
			Name: "cool30",
			GPUs: []inventory.GPUSpec{
				{Name: "NVIDIA GeForce RTX 2080 Ti", Class: "rtx2080ti", Memory: "11GB", Indices: []int{0}},
			},
		},
	})
	t.Cleanup(restore)

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "python train.py", "retry mismatch")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, "ampere+"); err != nil {
		t.Fatalf("set gpu class: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusFailed, jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	err = restartJob(database, jobID, restartOverrides{})
	if err == nil {
		t.Fatal("expected restart to be rejected")
	}
	if got, want := err.Error(), "gpu gate: no GPU matching class ampere+"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestParseRestartOverrides_ParsesGPUAndMem(t *testing.T) {
	restartGPU = "nvidia>=24GB"
	restartGPUClass = ""
	restartGPUMem = 0
	restartGPUMemStrict = false

	cmd := &cobra.Command{Use: "retry"}
	addRestartFlags(cmd)
	if err := cmd.Flags().Set("gpu", "nvidia>=24GB"); err != nil {
		t.Fatalf("set gpu flag: %v", err)
	}

	overrides, err := parseRestartOverrides(cmd)
	if err != nil {
		t.Fatalf("parseRestartOverrides: %v", err)
	}
	if overrides.GPUClass != "nvidia" {
		t.Fatalf("GPUClass = %q, want nvidia", overrides.GPUClass)
	}
	if overrides.GPUMemGB == nil || *overrides.GPUMemGB != 24 {
		t.Fatalf("GPUMemGB = %v, want 24", overrides.GPUMemGB)
	}
	if !overrides.GPUMemHardwareFloor {
		t.Fatalf("GPUMemHardwareFloor = false, want true")
	}
}

func TestParseRestartOverrides_SeparateGPUMemAddsHeadroom(t *testing.T) {
	restartGPU = ""
	restartGPUClass = "nvidia"
	restartGPUMem = 24
	restartGPUMemStrict = false

	cmd := &cobra.Command{Use: "retry"}
	addRestartFlags(cmd)
	if err := cmd.Flags().Set("gpu-class", "nvidia"); err != nil {
		t.Fatalf("set gpu-class flag: %v", err)
	}
	if err := cmd.Flags().Set("gpu-mem", "24"); err != nil {
		t.Fatalf("set gpu-mem flag: %v", err)
	}

	overrides, err := parseRestartOverrides(cmd)
	if err != nil {
		t.Fatalf("parseRestartOverrides: %v", err)
	}
	if overrides.GPUMemGB == nil || *overrides.GPUMemGB != 26 {
		t.Fatalf("GPUMemGB = %v, want 26 (24 + headroom)", overrides.GPUMemGB)
	}
	if overrides.GPUMemHardwareFloor {
		t.Fatalf("GPUMemHardwareFloor = true, want false")
	}
}

func TestParseRestartOverrides_StrictKeepsExactMem(t *testing.T) {
	restartGPU = "nvidia>=24GB"
	restartGPUClass = ""
	restartGPUMem = 0
	restartGPUMemStrict = true

	cmd := &cobra.Command{Use: "retry"}
	addRestartFlags(cmd)
	if err := cmd.Flags().Set("gpu", "nvidia>=24GB"); err != nil {
		t.Fatalf("set gpu flag: %v", err)
	}
	if err := cmd.Flags().Set("gpu-mem-strict", "true"); err != nil {
		t.Fatalf("set gpu-mem-strict flag: %v", err)
	}

	overrides, err := parseRestartOverrides(cmd)
	if err != nil {
		t.Fatalf("parseRestartOverrides: %v", err)
	}
	if !overrides.GPUMemStrict {
		t.Fatalf("GPUMemStrict = false, want true")
	}
	if overrides.GPUMemGB == nil || *overrides.GPUMemGB != 24 {
		t.Fatalf("GPUMemGB = %v, want 24", overrides.GPUMemGB)
	}
}

func TestParseRestartOverrides_MinSurvivalZero(t *testing.T) {
	restartGPU = ""
	restartGPUClass = ""
	restartGPUMem = 0

	cmd := &cobra.Command{Use: "retry"}
	addRestartFlags(cmd)
	if err := cmd.Flags().Set("min-survival", "0"); err != nil {
		t.Fatalf("set min-survival flag: %v", err)
	}

	overrides, err := parseRestartOverrides(cmd)
	if err != nil {
		t.Fatalf("parseRestartOverrides: %v", err)
	}
	if !overrides.HasMinSurvival {
		t.Fatalf("HasMinSurvival = false, want true")
	}
	if overrides.MinSurvival != 0 {
		t.Fatalf("MinSurvival = %v, want 0", overrides.MinSurvival)
	}
	if !overrides.HasAny {
		t.Fatalf("HasAny = false, want true")
	}
}

func TestParseRestartOverrides_RejectsInvalidMinSurvival(t *testing.T) {
	cmd := &cobra.Command{Use: "retry"}
	addRestartFlags(cmd)
	if err := cmd.Flags().Set("min-survival", "1.5"); err != nil {
		t.Fatalf("set min-survival flag: %v", err)
	}

	_, err := parseRestartOverrides(cmd)
	if err == nil {
		t.Fatal("parseRestartOverrides succeeded, want error")
	}
}

func TestRestartQueuedJob_UpdatesGPUOverrides(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobPlacementReasons(database, jobID, []string{"planner: no offers from providers for gpu=A100 vram>=82GB"}); err != nil {
		t.Fatalf("set placement reasons: %v", err)
	}
	overrides := restartOverrides{
		GPUClass:  "nvidia",
		GPUMemGB:  intPtrRestart(24),
		HasAny:    true,
		HasGPUMem: true,
	}

	if err := restartJob(database, jobID, overrides); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !strings.EqualFold(job.GPUClass, "nvidia") {
		t.Fatalf("GPUClass = %q, want nvidia", job.GPUClass)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 24 {
		t.Fatalf("GPUMemGB = %v, want 24", job.GPUMemGB)
	}
	if len(job.PlacementReasons) != 0 {
		t.Fatalf("PlacementReasons = %v, want cleared", job.PlacementReasons)
	}
}

func TestRestartQueuedJob_ReappliesScriptGPUMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-mem = 24
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	oldMem := 8
	if err := db.SetJobGPUMemGB(database, jobID, &oldMem); err != nil {
		t.Fatalf("set gpu mem: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 26 {
		t.Fatalf("GPUMemGB = %v, want 26 (24 + headroom)", job.GPUMemGB)
	}
}

func TestRestartQueuedJob_OverrideBeatsScriptMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-mem = 24
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	overrides := restartOverrides{
		GPUMemGB:  intPtrRestart(32),
		HasAny:    true,
		HasGPUMem: true,
	}

	if err := restartJob(database, jobID, overrides); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 32 {
		t.Fatalf("GPUMemGB = %v, want 34 (32 + headroom)", job.GPUMemGB)
	}
}

func TestRestartQueuedJob_StrictKeepsExactGPUMem(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-mem = 24
# gpu-mem-strict = true
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 24 {
		t.Fatalf("GPUMemGB = %v, want 24", job.GPUMemGB)
	}
}

func TestRestartUnplacedRentalJob_ResetsToQueued(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a rental job, assign it to an instance, then clear the instance
	// (simulating instance termination). Retry should reset to unplaced.
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "orphan-retry", "nvidia")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.AddJobTag(database, jobID, db.TagRental); err != nil {
		t.Fatalf("add rental tag: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set launch: %v", err)
	}
	// Mark the attempt as failed
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusFailed, 1000, jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	// Clear the launch (simulates instance termination cleanup)
	if _, err := database.Exec(`UPDATE job_attempts SET launch_id = NULL WHERE job_id = ?`, jobID); err != nil {
		t.Fatalf("clear launch: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.EffectiveStatus() != db.StatusQueued {
		t.Fatalf("status = %q, want %q", job.EffectiveStatus(), db.StatusQueued)
	}
	if job.Host != "" {
		t.Fatalf("host = %q, want empty", job.Host)
	}
	if job.LaunchID != nil {
		t.Fatalf("launch_id = %v, want nil", job.LaunchID)
	}
}

func TestRestartUnplacedFailedJobWithoutRentalTag_ResetsToQueued(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "orphan-retry-untagged")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusFailed, 1000, jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.EffectiveStatus() != db.StatusQueued {
		t.Fatalf("status = %q, want %q", job.EffectiveStatus(), db.StatusQueued)
	}
	if job.Host != "" {
		t.Fatalf("host = %q, want empty", job.Host)
	}
}

func intPtrRestart(v int) *int {
	return &v
}

// TestRestartQueuedJob_ClearsGPUMemWhenScriptDropsIt regression-tests the bug
// where a job with gpu_mem inherited from the script's PEP 723 block would
// keep its stale value on retry after the script author removed the gpu-mem
// line. No CLI override was stored, so on retry the merge produces no
// gpu-mem and the DB field is cleared.
func TestRestartQueuedJob_ClearsGPUMemWhenScriptDropsIt(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	// Script still has a [tool.weft] block (tags) so meta != nil, but no
	// longer declares gpu-mem.
	script := `# /// script
# [tool.weft]
# tags = ["benchmark-isolation"]
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	stale := 82
	if err := db.SetJobGPUMemGB(database, jobID, &stale); err != nil {
		t.Fatalf("set stale gpu mem: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB != nil {
		t.Fatalf("GPUMemGB = %v, want nil (script no longer declares gpu-mem)", *job.GPUMemGB)
	}
}

// TestRestartQueuedJob_PreservesStoredCLIOverride verifies that when the user
// submitted a job with an explicit --gpu-mem CLI flag, that intent is
// replayed on retry even if the script's gpu-mem has changed or been removed.
func TestRestartQueuedJob_PreservesStoredCLIOverride(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	// Script no longer declares gpu-mem.
	script := `# /// script
# [tool.weft]
# tags = ["benchmark-isolation"]
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	// The user originally passed --gpu-mem=40 on the CLI; post-headroom the
	// effective value was 42. Record both.
	eff := 42
	if err := db.SetJobGPUMemGB(database, jobID, &eff); err != nil {
		t.Fatalf("set gpu mem: %v", err)
	}
	raw := 40
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{
		GPUMemGB: &raw,
	}); err != nil {
		t.Fatalf("set cli overrides: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 42 {
		t.Fatalf("GPUMemGB = %v, want 42 (CLI override replayed)", job.GPUMemGB)
	}
}

// TestRestartQueuedJob_ReplaysCLIOverrideWithChangedScript verifies the
// merge: when script gpu-mem changes and the user had a CLI override, the
// CLI override still wins.
func TestRestartQueuedJob_ReplaysCLIOverrideWithChangedScript(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-mem = 16
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	// Stored CLI override: user passed --gpu-mem=40 at submission.
	raw := 40
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{
		GPUMemGB: &raw,
	}); err != nil {
		t.Fatalf("set cli overrides: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 42 {
		t.Fatalf("GPUMemGB = %v, want 42 (CLI 40 + headroom)", job.GPUMemGB)
	}
}

func TestRestartQueuedJob_ReplaysCLIDiskOverrideWithChangedScript(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# disk = 80
# runtime-disk = 12
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("create queued attempt: %v", err)
	}
	cliDisk := 200
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{
		DiskGB: &cliDisk,
	}); err != nil {
		t.Fatalf("set cli overrides: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Metadata == nil || job.Metadata.Disk == nil {
		t.Fatalf("disk metadata missing, want CLI disk plus script runtime")
	}
	if job.Metadata.Disk.DiskGB != 200 {
		t.Fatalf("DiskGB = %d, want 200 from CLI override", job.Metadata.Disk.DiskGB)
	}
	if job.Metadata.Disk.RuntimeDiskGB != 12 {
		t.Fatalf("RuntimeDiskGB = %d, want 12 from current script metadata", job.Metadata.Disk.RuntimeDiskGB)
	}
}

func TestRestartQueuedJob_RereadsScriptDiskWhenNoCLIOverride(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# disk = 96
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("create queued attempt: %v", err)
	}
	if err := db.SetJobMetadata(database, jobID, &db.JobMetadata{
		Disk: &db.JobDiskMetadata{DiskGB: 50},
	}); err != nil {
		t.Fatalf("set stale disk metadata: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Metadata == nil || job.Metadata.Disk == nil || job.Metadata.Disk.DiskGB != 96 {
		t.Fatalf("disk metadata = %+v, want current script disk 96", job.Metadata)
	}
}

func TestRestartQueuedWithCloudHistory_CarriesDiskToFreshAttempt(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# runtime-disk = 8
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry reset")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	cliDisk := 200
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{
		DiskGB: &cliDisk,
	}); err != nil {
		t.Fatalf("set cli overrides: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	attempt1, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusCanceled)
	if err != nil {
		t.Fatalf("create cloud attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = ?, end_time = ?, status = ?, cloud_outcome = ? WHERE id = ?`,
		1_699_999_000, 1_700_000_000, db.StatusCanceled, db.AttemptOutcomeOrphaned, attempt1); err != nil {
		t.Fatalf("close cloud attempt: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("create open queued attempt: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued-with-cloud-history failed: %v", err)
	}

	var raw string
	if err := database.QueryRow(`
		SELECT COALESCE(job_metadata, '')
		FROM job_attempts
		WHERE job_id = ?
		ORDER BY attempt_number DESC
		LIMIT 1`, jobID).Scan(&raw); err != nil {
		t.Fatalf("query latest metadata: %v", err)
	}
	if !strings.Contains(raw, `"disk_gb":200`) {
		t.Fatalf("latest metadata = %s, want disk_gb 200", raw)
	}
	if !strings.Contains(raw, `"runtime_disk_gb":8`) {
		t.Fatalf("latest metadata = %s, want runtime_disk_gb 8", raw)
	}
}

func TestRestartQueuedJob_PersistsRetryGPUClassOverride(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-class = "4090"
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, "4090"); err != nil {
		t.Fatalf("set gpu class: %v", err)
	}
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{
		GPUClass: "4090",
	}); err != nil {
		t.Fatalf("set cli overrides: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{GPUClass: "ampere+", HasAny: true}); err != nil {
		t.Fatalf("restartJob with override failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job after override: %v", err)
	}
	if job.GPUClass != "ampere+" {
		t.Fatalf("GPUClass = %q, want ampere+ after retry override", job.GPUClass)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.GPUClass != "ampere+" {
		t.Fatalf("CLI GPUClass override = %+v, want ampere+", job.CLIResourceOverrides)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob without override failed: %v", err)
	}
	job, err = db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job after second retry: %v", err)
	}
	if job.GPUClass != "ampere+" {
		t.Fatalf("GPUClass = %q, want ampere+ after later retry", job.GPUClass)
	}
}

func TestRestartQueuedJob_PersistsMinSurvivalOverride(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobPlacementReasons(database, jobID, []string{"skipped A10 offers below survival threshold"}); err != nil {
		t.Fatalf("set placement reasons: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{
		HasAny:         true,
		HasMinSurvival: true,
		MinSurvival:    0,
	}); err != nil {
		t.Fatalf("restartJob with min-survival override failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.MinSurvival == nil {
		t.Fatalf("CLI min-survival override missing: %+v", job.CLIResourceOverrides)
	}
	if *job.CLIResourceOverrides.MinSurvival != 0 {
		t.Fatalf("MinSurvival = %v, want 0", *job.CLIResourceOverrides.MinSurvival)
	}
	if len(job.PlacementReasons) != 0 {
		t.Fatalf("PlacementReasons = %v, want cleared stale reasons", job.PlacementReasons)
	}
}

// TestRestartQueuedJob_NoScriptMetaLeavesFieldsAlone verifies that when the
// script has no PEP 723 block, retry does not clear existing resource fields.
// Protects against regressions where legacy jobs with pinned host/class get
// silently cleared.
func TestRestartQueuedJob_NoScriptMetaLeavesFieldsAlone(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, "ampere+"); err != nil {
		t.Fatalf("set gpu class: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUClass != "ampere+" {
		t.Fatalf("GPUClass = %q, want ampere+ (preserved when no script meta)", job.GPUClass)
	}
}

// Regression: a script with only PEP 723 `dependencies = [...]` (no
// [tool.weft] GPU block) must not clear the job's GPU fields on restart.
// Before the fix, ScanScriptMeta returned a non-nil meta with empty GPU fields,
// and applyScriptGPUDefaults' `meta == nil` guard no longer fired, so the
// effective GPU values fell back to empty and the job's GPUClass was cleared.
func TestRestartQueuedJob_DepsOnlyMetaLeavesGPUAlone(t *testing.T) {
	database := db.SetupTestDB(t)
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "train.py")
	script := `# /// script
# requires-python = ">=3.10"
# dependencies = ["torch"]
# ///
import torch
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", dir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, "ampere+"); err != nil {
		t.Fatalf("set gpu class: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUClass != "ampere+" {
		t.Fatalf("GPUClass = %q, want ampere+ (preserved when script has only PEP 723 dependencies)", job.GPUClass)
	}
}
