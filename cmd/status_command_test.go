package cmd

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
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

func TestRunStatusInventorySyncUsesBoundedStatusTimeout(t *testing.T) {
	stubEmptyQueueStatus(t)

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "studio", "/tmp", "echo hi", "bounded status", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreStatusFlags(t)
	statusSync = true

	originalSyncJob := syncJobForDisplayFunc
	t.Cleanup(func() {
		syncJobForDisplayFunc = originalSyncJob
	})

	var gotJobID int64
	var gotTimeout time.Duration
	var gotSkipSamples bool
	syncJobForDisplayFunc = func(_ *sql.DB, job *db.Job, opts ops.SyncOptions) (ops.SyncResult, error) {
		gotJobID = job.ID
		gotTimeout = opts.Timeout
		gotSkipSamples = opts.SkipSamples
		return ops.SyncResult{HostContacted: true}, nil
	}

	captureStdout(t, func() {
		if err := runStatus(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})

	if gotJobID != jobID {
		t.Fatalf("synced job = %d, want %d", gotJobID, jobID)
	}
	if gotTimeout != NormalSyncTimeout {
		t.Fatalf("sync timeout = %v, want %v", gotTimeout, NormalSyncTimeout)
	}
	if !gotSkipSamples {
		t.Fatal("targeted read-only status should skip sample collection")
	}
}

func TestRunStatusSkipsLiveSyncWhenDaemonSyncIsFresh(t *testing.T) {
	stubEmptyQueueStatus(t)

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "studio", "/tmp", "echo hi", "fresh status", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.RecordHostSync(database, "studio", time.Now()); err != nil {
		t.Fatalf("RecordHostSync: %v", err)
	}

	restoreStatusFlags(t)
	daemonLiveFunc = func() bool { return true }

	originalSyncJob := syncJobForDisplayFunc
	t.Cleanup(func() {
		syncJobForDisplayFunc = originalSyncJob
	})

	calls := 0
	syncJobForDisplayFunc = func(_ *sql.DB, _ *db.Job, _ ops.SyncOptions) (ops.SyncResult, error) {
		calls++
		return ops.SyncResult{HostContacted: true}, nil
	}

	captureStdout(t, func() {
		if err := runStatus(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})

	if calls != 0 {
		t.Fatalf("status sync calls = %d, want 0 with fresh daemon sync", calls)
	}
}

func TestRunStatusForceSyncIgnoresFreshDaemonSync(t *testing.T) {
	stubEmptyQueueStatus(t)

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "studio", "/tmp", "echo hi", "forced status", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.RecordHostSync(database, "studio", time.Now()); err != nil {
		t.Fatalf("RecordHostSync: %v", err)
	}

	restoreStatusFlags(t)
	statusSync = true
	daemonLiveFunc = func() bool { return true }

	originalSyncJob := syncJobForDisplayFunc
	t.Cleanup(func() {
		syncJobForDisplayFunc = originalSyncJob
	})

	calls := 0
	syncJobForDisplayFunc = func(_ *sql.DB, _ *db.Job, _ ops.SyncOptions) (ops.SyncResult, error) {
		calls++
		return ops.SyncResult{HostContacted: true}, nil
	}

	captureStdout(t, func() {
		if err := runStatus(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})

	if calls != 1 {
		t.Fatalf("targeted sync calls = %d, want 1 with --sync", calls)
	}
}

func TestShowActiveJobsDefaultDoesNotSyncHosts(t *testing.T) {
	stubEmptyQueueStatus(t)

	database := db.SetupTestDB(t)
	if _, err := db.RecordQueuedWithGPU(database, "studio", "/tmp", "echo hi", "active status", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreStatusFlags(t)
	daemonLiveFunc = func() bool { return false }

	originalSyncHosts := statusSyncHostsFunc
	t.Cleanup(func() {
		statusSyncHostsFunc = originalSyncHosts
	})

	calls := 0
	statusSyncHostsFunc = func(_ *sql.DB, _ []string, _, _ time.Duration, _ bool) (bool, []string, []string) {
		calls++
		return true, nil, nil
	}

	captureStdout(t, func() {
		if err := showActiveJobs(database); err != nil {
			t.Fatalf("showActiveJobs: %v", err)
		}
	})

	if calls != 0 {
		t.Fatalf("default no-arg status sync calls = %d, want 0", calls)
	}
}

func TestShowActiveJobsSyncUsesBoundedSyncWithoutStartingQueueRunners(t *testing.T) {
	stubEmptyQueueStatus(t)

	database := db.SetupTestDB(t)
	if _, err := db.RecordQueuedWithGPU(database, "studio", "/tmp", "echo hi", "active status", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreStatusFlags(t)
	statusSync = true

	originalSyncHosts := statusSyncHostsFunc
	t.Cleanup(func() {
		statusSyncHostsFunc = originalSyncHosts
	})

	calls := 0
	var gotStartQueueRunners bool
	statusSyncHostsFunc = func(_ *sql.DB, hosts []string, sshTimeout, hostTimeout time.Duration, startQueueRunners bool) (bool, []string, []string) {
		calls++
		if strings.Join(hosts, ",") != "studio" {
			t.Fatalf("synced hosts = %v, want [studio]", hosts)
		}
		if sshTimeout != NormalSyncTimeout {
			t.Fatalf("ssh timeout = %v, want %v", sshTimeout, NormalSyncTimeout)
		}
		if hostTimeout != NormalSyncTimeout {
			t.Fatalf("host timeout = %v, want %v", hostTimeout, NormalSyncTimeout)
		}
		gotStartQueueRunners = startQueueRunners
		return true, nil, nil
	}

	captureStdout(t, func() {
		if err := showActiveJobs(database); err != nil {
			t.Fatalf("showActiveJobs: %v", err)
		}
	})

	if calls != 1 {
		t.Fatalf("status sync calls = %d, want 1", calls)
	}
	if gotStartQueueRunners {
		t.Fatal("no-arg status should not start queue runners")
	}
}

func TestStatusHostSyncBoundsRespectSSHTimeoutOverride(t *testing.T) {
	restoreStatusFlags(t)
	statusSSHTimeout = 2 * time.Minute

	sshTimeout, hostTimeout := statusHostSyncBounds()
	if sshTimeout != statusSSHTimeout {
		t.Fatalf("ssh timeout = %v, want %v", sshTimeout, statusSSHTimeout)
	}
	if hostTimeout != statusSSHTimeout {
		t.Fatalf("host timeout = %v, want %v", hostTimeout, statusSSHTimeout)
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
	var gotHostTimeout time.Duration
	var gotCloudTimeout time.Duration
	quickSyncJobsFunc = func(_ *sql.DB, jobs []*db.Job, sshTimeout, hostTimeout, cloudTimeout time.Duration) targetedSyncOutcome {
		if len(jobs) != 1 {
			t.Fatalf("quickSyncJobs got %d jobs, want 1", len(jobs))
		}
		if jobs[0].ID != jobID {
			t.Fatalf("quickSyncJobs job ID = %d, want %d", jobs[0].ID, jobID)
		}
		gotSSHTimeout = sshTimeout
		gotHostTimeout = hostTimeout
		gotCloudTimeout = cloudTimeout
		return targetedSyncOutcome{}
	}

	captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})

	if gotSSHTimeout != NormalSyncTimeout {
		t.Fatalf("job info SSH timeout = %v, want %v", gotSSHTimeout, NormalSyncTimeout)
	}
	if gotHostTimeout != FastSyncHostTimeout {
		t.Fatalf("job info host timeout = %v, want %v", gotHostTimeout, FastSyncHostTimeout)
	}
	if gotCloudTimeout != NormalCloudSyncTimeout {
		t.Fatalf("job info cloud timeout = %v, want %v", gotCloudTimeout, NormalCloudSyncTimeout)
	}
}

func TestRunJobInfoSkipsQuickSyncWhenDaemonSyncIsFresh(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID := createRentalQueuedJob(t, database)
	if err := db.RecordCloudSync(database, time.Now()); err != nil {
		t.Fatalf("RecordCloudSync: %v", err)
	}

	restoreJobInfoFlags(t)
	daemonLiveFunc = func() bool { return true }

	originalQuickSyncJobs := quickSyncJobsFunc
	t.Cleanup(func() {
		quickSyncJobsFunc = originalQuickSyncJobs
	})

	calls := 0
	quickSyncJobsFunc = func(_ *sql.DB, _ []*db.Job, _, _, _ time.Duration) targetedSyncOutcome {
		calls++
		return targetedSyncOutcome{}
	}

	captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})

	if calls != 0 {
		t.Fatalf("quickSyncJobs calls = %d, want 0 with fresh daemon sync", calls)
	}
}

func TestInstanceStatusSkipsLiveRefreshWhenDaemonCloudSyncIsFresh(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, _ := createRentalQueuedJobWithInstance(t, database)
	if err := db.RecordCloudSync(database, time.Now()); err != nil {
		t.Fatalf("RecordCloudSync: %v", err)
	}

	restoreInstanceStatusFlags(t)
	daemonLiveFunc = func() bool { return true }

	out := captureStdout(t, func() {
		if err := runInstanceStatus(&cobra.Command{}, []string{ids.FormatInstanceID(instanceID)}); err != nil {
			t.Fatalf("runInstanceStatus: %v", err)
		}
	})

	if !strings.Contains(out, "Instance: (provisioning") {
		t.Fatalf("expected cached instance display without provider status, got:\n%s", out)
	}
}

func TestInstanceStatusNoSyncSkipsLiveRefresh(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, _ := createRentalQueuedJobWithInstance(t, database)

	restoreInstanceStatusFlags(t)
	instanceStatusNoSync = true
	daemonLiveFunc = func() bool { return false }

	out := captureStdout(t, func() {
		if err := runInstanceStatus(&cobra.Command{}, []string{ids.FormatInstanceID(instanceID)}); err != nil {
			t.Fatalf("runInstanceStatus: %v", err)
		}
	})

	if !strings.Contains(out, "Instance: (provisioning") {
		t.Fatalf("expected --no-sync cached instance display, got:\n%s", out)
	}
}

func TestShouldRefreshInstanceLiveStateUsesDaemonCloudFreshness(t *testing.T) {
	database := db.SetupTestDB(t)
	launch := &db.Launch{Status: db.LaunchStatusRunning}
	if _, err := database.Exec(`DELETE FROM host_syncs WHERE name = ?`, db.CloudSyncTargetName); err != nil {
		t.Fatalf("clear cloud sync: %v", err)
	}
	if got := db.GetLastCloudSync(database); !got.IsZero() {
		t.Fatalf("cloud sync after clear = %v, want zero", got)
	}
	if campaign.IsInstanceTerminal(launch.Status) {
		t.Fatalf("launch status %q unexpectedly terminal", launch.Status)
	}

	restoreInstanceStatusFlags(t)
	daemonLiveFunc = func() bool { return true }

	if !shouldRefreshInstanceLiveState(database, launch, false, false) {
		t.Fatal("missing cloud sync should require live refresh")
	}
	if err := db.RecordCloudSync(database, time.Now()); err != nil {
		t.Fatalf("RecordCloudSync: %v", err)
	}
	if shouldRefreshInstanceLiveState(database, launch, false, false) {
		t.Fatal("fresh daemon cloud sync should skip live refresh")
	}
	if !shouldRefreshInstanceLiveState(database, launch, true, false) {
		t.Fatal("--sync should force live refresh")
	}
	if shouldRefreshInstanceLiveState(database, launch, true, true) {
		t.Fatal("--no-sync should skip live refresh")
	}
}

func TestRunJobInfoFormatsJobIDWithPrefix(t *testing.T) {
	stubEmptyQueueStatus(t)

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp", "echo hi", "id format", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true

	out := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})
	want := fmt.Sprintf("Job ID:      %s", ids.FormatJobID(jobID))
	if !strings.Contains(out, want) {
		t.Fatalf("missing formatted job ID %q, got:\n%s", want, out)
	}
}

func TestRunJobInfoUnplacedQueuedStatusIsDisambiguated(t *testing.T) {
	stubEmptyQueueStatus(t)

	database := db.SetupTestDB(t)

	// Unplaced job: no host, no launch. Bare "queued" next to an attempt
	// table reads as "queued on <last attempt's instance>"; the status
	// line must spell out that the job is not placed.
	unplacedID, err := db.RecordQueued(database, "", "/tmp", "echo hi", "unplaced job")
	if err != nil {
		t.Fatalf("RecordQueued (unplaced): %v", err)
	}
	// Placed job: queued on an inventory host. "queued" is unambiguous
	// here because the Host line names the target, so it stays bare.
	placedID, err := db.RecordQueued(database, "cool30", "/tmp", "echo hi", "placed job")
	if err != nil {
		t.Fatalf("RecordQueued (placed): %v", err)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true

	unplacedOut := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(unplacedID)}); err != nil {
			t.Fatalf("runJobInfo (unplaced): %v", err)
		}
	})
	if !strings.Contains(unplacedOut, "Status:      queued (unplaced — awaiting placement)") {
		t.Fatalf("unplaced job: status line not disambiguated, got:\n%s", unplacedOut)
	}

	placedOut := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(placedID)}); err != nil {
			t.Fatalf("runJobInfo (placed): %v", err)
		}
	})
	if !strings.Contains(placedOut, "Status:      queued\n") {
		t.Fatalf("placed job: status line should stay bare, got:\n%s", placedOut)
	}
}

func TestRunJobInfoShowsStructuredPlacementBreakdown(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp", "python train.py", "blocked unplaced")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	flat := "no rental headroom; running instances couldn't accept this job: disk insufficient"
	if err := db.SetJobPlacementReasons(database, jobID, []string{flat}); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}
	// A relaunch.skipped event makes the job render as blocked (queueblock
	// hydration), which is the gate for the Reason breakdown.
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchSkippedNoOffers,
		JobID:     jobID,
		Detail:    flat,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}
	structured := (&blockreason.Structured{
		Summary: flat,
		Launch:  "no rental headroom",
		Reuse: []blockreason.ReuseRejection{
			{Instance: "wi1023", Reason: "disk insufficient: need=42GB free=12GB"},
			{Instance: "wi1044", Reason: "GPU class mismatch: job=ampere instance=ada"},
		},
	}).Marshal()
	if err := db.SetJobPlacementBlocked(database, jobID, structured); err != nil {
		t.Fatalf("SetJobPlacementBlocked: %v", err)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true

	out := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})
	for _, want := range []string{
		"Reason:      " + flat,
		"new instance  no rental headroom",
		"reuse wi1023  disk insufficient: need=42GB free=12GB",
		"reuse wi1044  GPU class mismatch: job=ampere instance=ada",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
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

func TestRunStatusHidesMissingPayloadDetailWithoutReportingBug(t *testing.T) {
	database := db.SetupTestDB(t)
	bugDatabase := db.SetupTestBugDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp", "echo hi", "blocked status", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreStatusFlags(t)
	statusNoSync = true

	rawReason := "missing queue payload: open /Users/agent/.cache/weft/queue/job-42.json: no such file or directory"
	cleanupSSH := ssh.SetRunner(func(_ string, _ string) (string, string, error) {
		return fmt.Sprintf("RUNNER:yes\nCURRENT:\nDEPTH:1\nBLOCKED:%d:%s\nSTOP:no\n", jobID, rawReason), "", nil
	})
	t.Cleanup(cleanupSSH)

	out := captureStdout(t, func() {
		if err := runStatus(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})
	if strings.Contains(out, "missing queue payload") || strings.Contains(out, "job-42.json") {
		t.Fatalf("raw invariant leaked to user:\n%s", out)
	}
	if strings.Contains(out, "Weft bug wb") {
		t.Fatalf("unexpected bug id in output:\n%s", out)
	}
	if !strings.Contains(out, "temporarily inconsistent") {
		t.Fatalf("missing temporary inconsistency explanation:\n%s", out)
	}
	bugs, err := db.ListBugs(bugDatabase, false)
	if err != nil {
		t.Fatalf("ListBugs: %v", err)
	}
	if len(bugs) != 0 {
		t.Fatalf("len(bugs) = %d, want 0", len(bugs))
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

func TestRunJobInfoRentalDisplaysHostElapsedEstimateETAAndCost(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().Unix()
	launchedAt := now - 3600
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUSpec:          "RTX_4090",
		CostPerHourCents: 100,
		LaunchedAt:       &launchedAt,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, db.LaunchHost(instanceID), "/tmp", "echo hi", "info output", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusRunning, now-900, jobID); err != nil {
		t.Fatalf("set running attempt: %v", err)
	}
	if err := db.SetJobPlacementMeta(database, jobID, &db.PlacementMeta{PredictedDurationS: float64PtrSC(1800)}); err != nil {
		t.Fatalf("SetJobPlacementMeta: %v", err)
	}
	if err := db.UpsertJobPhaseTimings(database, &db.JobPhaseTimings{
		JobID:      jobID,
		SetupStart: int64PtrSC(now - 930),
		SetupEnd:   int64PtrSC(now - 900),
		RunStart:   int64PtrSC(now - 900),
	}); err != nil {
		t.Fatalf("UpsertJobPhaseTimings: %v", err)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true

	out := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})
	if !strings.Contains(out, "Host:        wi") {
		t.Fatalf("missing wi host label, got:\n%s", out)
	}
	if !strings.Contains(out, "Elapsed:") {
		t.Fatalf("missing elapsed line, got:\n%s", out)
	}
	if !strings.Contains(out, "Est. Time:") {
		t.Fatalf("missing estimate line, got:\n%s", out)
	}
	if !strings.Contains(out, "ETA:") {
		t.Fatalf("missing ETA line, got:\n%s", out)
	}
	if !strings.Contains(out, "Cost:") || !strings.Contains(out, "instance total") {
		t.Fatalf("missing instance-total cost line, got:\n%s", out)
	}
}

func TestRunStatusQueuedRentalShowsPlacementAndQueueReason(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, jobID := createRentalQueuedJobWithInstance(t, database)

	restoreStatusFlags(t)
	statusNoSync = true

	out := captureStdout(t, func() {
		if err := runStatus(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runStatus: %v", err)
		}
	})
	if !strings.Contains(out, fmt.Sprintf("Placement: assigned to wi%d", instanceID)) {
		t.Fatalf("missing placement line, got:\n%s", out)
	}
	if !strings.Contains(out, "Queue reason: waiting for assigned target to start the job") {
		t.Fatalf("missing queue reason, got:\n%s", out)
	}
}

func TestRunJobInfoQueuedUnplacedShowsPlacement(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "unplaced", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true

	out := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})
	if !strings.Contains(out, "Placement:   unplaced, awaiting assignment") {
		t.Fatalf("missing unplaced placement line, got:\n%s", out)
	}
}

func TestRunJobInfoQueuedRentalBehindRunningJobShowsQueueReason(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, queuedID := createRentalQueuedJobWithInstance(t, database)
	runningID, err := db.RecordQueuedWithGPU(database, db.LaunchHost(instanceID), "/tmp", "echo run", "running", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(running): %v", err)
	}
	if err := db.SetJobLaunchID(database, runningID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(running): %v", err)
	}
	now := time.Now().Unix()
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusRunning, now, runningID); err != nil {
		t.Fatalf("set running attempt: %v", err)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true

	out := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(queuedID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})
	if !strings.Contains(out, fmt.Sprintf("Queue reason: waiting behind %s", ids.FormatJobID(runningID))) {
		t.Fatalf("missing behind-running queue reason, got:\n%s", out)
	}
}

func TestRunJobInfoRentalSharedInstanceUsesSetupAndRunCostBasis(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().Unix()
	launchedAt := now - 1800
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUSpec:          "RTX_4090",
		CostPerHourCents: 240,
		LaunchedAt:       &launchedAt,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, db.LaunchHost(instanceID), "/tmp", "echo hi", "shared billing", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusRunning, now-600, jobID); err != nil {
		t.Fatalf("set running attempt: %v", err)
	}
	if err := db.UpsertJobPhaseTimings(database, &db.JobPhaseTimings{
		JobID:      jobID,
		SetupStart: int64PtrSC(now - 660),
		SetupEnd:   int64PtrSC(now - 600),
		RunStart:   int64PtrSC(now - 600),
	}); err != nil {
		t.Fatalf("UpsertJobPhaseTimings: %v", err)
	}

	otherID, err := db.RecordQueuedWithGPU(database, db.LaunchHost(instanceID), "/tmp", "echo other", "other job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(other): %v", err)
	}
	if err := db.SetJobLaunchID(database, otherID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(other): %v", err)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true

	out := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})
	if !strings.Contains(out, "Cost:") || !strings.Contains(out, "shared instance: setup + run") {
		t.Fatalf("missing shared-cost basis line, got:\n%s", out)
	}
}

func TestRunJobInfoCompletedQueuedIntentShowsCompletedAttemptTarget(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "completed target", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	if _, err := db.AssignJobHost(database, jobID, "cloud"); err != nil {
		t.Fatalf("AssignJobHost: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	exitCode := 0
	endTime := time.Now().Unix()
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exitCode, endTime); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	if err := db.CloseLaunchAttempt(database, jobID, db.AttemptOutcomeCompleted); err != nil {
		t.Fatalf("CloseLaunchAttempt: %v", err)
	}

	restoreJobInfoFlags(t)
	jobInfoNoSync = true

	out := captureStdout(t, func() {
		if err := runJobInfo(&cobra.Command{}, []string{fmt.Sprint(jobID)}); err != nil {
			t.Fatalf("runJobInfo: %v", err)
		}
	})
	if !strings.Contains(out, "Status:      completed") {
		t.Fatalf("missing completed status, got:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("Host:        wi%d", instanceID)) {
		t.Fatalf("missing completed attempt target, got:\n%s", out)
	}
}

func int64PtrSC(v int64) *int64 {
	return &v
}

func float64PtrSC(v float64) *float64 {
	return &v
}

func createRentalQueuedJob(t *testing.T, database *sql.DB) int64 {
	t.Helper()
	_, jobID := createRentalQueuedJobWithInstance(t, database)
	return jobID
}

func createRentalQueuedJobWithInstance(t *testing.T, database *sql.DB) (int64, int64) {
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
	return instanceID, jobID
}

func restoreStatusFlags(t *testing.T) {
	t.Helper()

	origSync := statusSync
	origNoSync := statusNoSync
	origFast := statusFast
	origWait := statusWait
	origWaitTimeout := statusWaitTimeout
	origSSHTimeout := statusSSHTimeout
	origDaemonLive := daemonLiveFunc

	t.Cleanup(func() {
		statusSync = origSync
		statusNoSync = origNoSync
		statusFast = origFast
		statusWait = origWait
		statusWaitTimeout = origWaitTimeout
		statusSSHTimeout = origSSHTimeout
		daemonLiveFunc = origDaemonLive
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
	origDaemonLive := daemonLiveFunc

	t.Cleanup(func() {
		jobInfoSync = origSync
		jobInfoNoSync = origNoSync
		daemonLiveFunc = origDaemonLive
	})

	jobInfoSync = false
	jobInfoNoSync = false
}

func restoreInstanceStatusFlags(t *testing.T) {
	t.Helper()

	origSync := instanceStatusSync
	origNoSync := instanceStatusNoSync
	origDaemonLive := daemonLiveFunc

	t.Cleanup(func() {
		instanceStatusSync = origSync
		instanceStatusNoSync = origNoSync
		daemonLiveFunc = origDaemonLive
	})

	instanceStatusSync = false
	instanceStatusNoSync = false
}
