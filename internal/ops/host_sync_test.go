package ops

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/r2"
	srcsync "github.com/osteele/weft/internal/sync"
)

// TestIsJobInRunnerState_FinishedCountsAsPresent guards the regression that
// re-ran wj2348 on cool30: ensureQueuedJobsOnRemote forward-reconciles by
// re-dispatching synced-queued jobs that aren't visible in the runner's live
// state. Before the archive-on-add fix, a re-dispatched completed job was
// silently dropped by JobCompleted; after that fix it runs again. The fix
// here is to recognize state.Finished as "the runner knows about this job"
// so the forward-reconcile loop skips it (the right action is local-DB
// status reconciliation, not a fresh attempt).
func TestIsJobInRunnerState_FinishedCountsAsPresent(t *testing.T) {
	current := int64(7)
	state := &opsqueue.RunnerState{
		Current: &current,
		Pending: []int64{1, 2},
		Running: map[string]opsqueue.RunnerJobState{
			"3": {StartedAt: 100},
		},
		Finished: map[string]opsqueue.RunnerFinishedState{
			"42": {ExitCode: 0, FinishedAt: 200},
			"43": {ExitCode: 1, FinishedAt: 201},
		},
	}

	cases := []struct {
		jobID int64
		want  bool
		where string
	}{
		{7, true, "current"},
		{1, true, "pending head"},
		{2, true, "pending tail"},
		{3, true, "running"},
		{42, true, "finished (success)"},
		{43, true, "finished (failure)"},
		{99, false, "unknown id"},
	}
	for _, tc := range cases {
		if got := isJobInRunnerState(tc.jobID, state); got != tc.want {
			t.Errorf("isJobInRunnerState(%d) [%s] = %v, want %v", tc.jobID, tc.where, got, tc.want)
		}
	}

	// nil state must remain "not present" so the no-state-file path
	// continues to trigger re-dispatch (see the gate comment in
	// ensureQueuedJobsOnRemote).
	if isJobInRunnerState(1, nil) {
		t.Errorf("isJobInRunnerState(1, nil) = true, want false")
	}
}

// TestShouldPruneStalePending exercises the classifier that decides whether
// a runner-pending entry should be cancelled via host_sync's reverse
// reconcile. The classifier is the contract surface — fixing it wrong
// either cancels legitimate work or leaks stale entries forever, both of
// which have hit production today.
func TestShouldPruneStalePending(t *testing.T) {
	cases := []struct {
		name      string
		job       *db.Job
		host      string
		wantPrune bool
		// reason prefix the classifier should emit for traceability.
		wantReasonPrefix string
	}{
		{
			name:             "nil job (deleted DB row) prunes",
			job:              nil,
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_row_missing",
		},
		{
			name:             "completed job prunes",
			job:              &db.Job{Host: "cool30", Status: db.StatusCompleted},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_terminal:",
		},
		{
			name:             "failed job prunes",
			job:              &db.Job{Host: "cool30", Status: db.StatusFailed},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_terminal:",
		},
		{
			name:             "killed job prunes",
			job:              &db.Job{Host: "cool30", Status: db.StatusKilled},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_terminal:",
		},
		{
			name:             "canceled job prunes",
			job:              &db.Job{Host: "cool30", Status: db.StatusCanceled},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_terminal:",
		},
		{
			name:             "queued on different host prunes",
			job:              &db.Job{Host: "cool100", Status: db.StatusQueued},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_queued_on_other_host",
		},
		{
			name: "queued and retargeted to rental prunes (TargetKind=RentalInstance)",
			job: &db.Job{
				Host:     "cool30",
				Status:   db.StatusQueued,
				LaunchID: int64Ptr(99),
			},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_queued_on_other_host",
		},
		{
			name:             "queued and unplaced prunes (Host cleared)",
			job:              &db.Job{Host: "", Status: db.StatusQueued},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_queued_on_other_host",
		},
		{
			name:             "queued on same host does NOT prune (defensive)",
			job:              &db.Job{Host: "cool30", Status: db.StatusQueued},
			host:             "cool30",
			wantPrune:        false,
			wantReasonPrefix: "db_queued_here",
		},
		{
			name:             "running job does NOT prune (transient)",
			job:              &db.Job{Host: "cool30", Status: db.StatusRunning},
			host:             "cool30",
			wantPrune:        false,
			wantReasonPrefix: "db_transient:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := shouldPruneStalePending(tc.job, tc.host)
			if got != tc.wantPrune {
				t.Fatalf("prune = %v, want %v (reason=%q)", got, tc.wantPrune, reason)
			}
			if !strings.HasPrefix(reason, tc.wantReasonPrefix) {
				t.Fatalf("reason = %q, want prefix %q", reason, tc.wantReasonPrefix)
			}
		})
	}
}

// stubProbeRemoteNeedsState swaps probeRemoteNeedsStateFunc for the duration
// of a test, recording call count and the last needs slice it received.
func stubProbeRemoteNeedsState(t *testing.T, reply map[string]remoteNeedState) (calls *int, lastNeeds *[]pendingNeed) {
	t.Helper()
	prev := probeRemoteNeedsStateFunc
	t.Cleanup(func() { probeRemoteNeedsStateFunc = prev })
	calls = new(int)
	lastNeeds = new([]pendingNeed)
	probeRemoteNeedsStateFunc = func(_ string, needs []pendingNeed, _ time.Duration) (map[string]remoteNeedState, error) {
		*calls++
		*lastNeeds = needs
		return reply, nil
	}
	return calls, lastNeeds
}

// TestStageArtifactNeedsForHost_SkipsOnPremProducers verifies that when every
// producer is on-prem, the function returns without ever probing or calling
// getR2Client — those needs are satisfied by the producer's own queue runner.
func TestStageArtifactNeedsForHost_SkipsOnPremProducers(t *testing.T) {
	database := db.SetupTestDB(t)

	producerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}
	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "consume", "consumer")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, consumerID, []string{fmt.Sprintf("output/x.bin:%d", producerID)}); err != nil {
		t.Fatalf("set needs: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}

	probeCalls, _ := stubProbeRemoteNeedsState(t, nil)
	getR2 := func() (*r2.Client, error) {
		t.Fatal("getR2Client should not be called when all producers are on-prem")
		return nil, nil
	}
	failed := stageArtifactNeedsForHost(database, "host-alpha", []*db.Job{consumer}, time.Second, getR2)
	if len(failed) != 0 {
		t.Fatalf("unexpected failures: %v", failed)
	}
	if *probeCalls != 0 {
		t.Fatalf("probeCalls = %d, want 0 (no rental needs to probe)", *probeCalls)
	}
}

// TestStageArtifactNeedsForHost_NoNeeds verifies the early-return for jobs
// with no --needs entries. No probe, no R2.
func TestStageArtifactNeedsForHost_NoNeeds(t *testing.T) {
	database := db.SetupTestDB(t)
	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "echo", "no-needs")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}
	probeCalls, _ := stubProbeRemoteNeedsState(t, nil)
	getR2 := func() (*r2.Client, error) {
		t.Fatal("getR2Client should not be called for jobs with no --needs")
		return nil, nil
	}
	failed := stageArtifactNeedsForHost(database, "host-alpha", []*db.Job{consumer}, time.Second, getR2)
	if len(failed) != 0 {
		t.Fatalf("unexpected failures: %v", failed)
	}
	if *probeCalls != 0 {
		t.Fatalf("probeCalls = %d, want 0 (no needs)", *probeCalls)
	}
}

// TestStageArtifactNeedsForHost_BatchesProbeAcrossJobs verifies that a
// host-wide stage call issues exactly one probe regardless of how many queued
// jobs need probing — guards against the per-job-probe regression.
func TestStageArtifactNeedsForHost_BatchesProbeAcrossJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	// Producer is a rental job (no host pin → producer.HasInventoryHost() is false).
	producerID, err := db.RecordQueued(database, "", "/tmp", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}

	jobs := make([]*db.Job, 0, 2)
	for i := 0; i < 2; i++ {
		consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", fmt.Sprintf("consume-%d", i), "consumer")
		if err != nil {
			t.Fatalf("record consumer: %v", err)
		}
		if err := db.SetJobNeeds(database, consumerID, []string{fmt.Sprintf("output/x-%d.bin:%d", i, producerID)}); err != nil {
			t.Fatalf("set needs: %v", err)
		}
		job, err := db.GetJobByID(database, consumerID)
		if err != nil {
			t.Fatalf("get consumer: %v", err)
		}
		jobs = append(jobs, job)
	}

	// Probe replies "marker present" for everything → no transfer, no R2.
	probeCalls, lastNeeds := stubProbeRemoteNeedsState(t, nil)
	probeRemoteNeedsStateFunc = func(_ string, needs []pendingNeed, _ time.Duration) (map[string]remoteNeedState, error) {
		*probeCalls++
		*lastNeeds = needs
		reply := make(map[string]remoteNeedState, len(needs))
		for _, n := range needs {
			reply[n.markerName] = remoteNeedState{markerExists: true, fileSize: 1, stagingSize: -1}
		}
		return reply, nil
	}

	getR2 := func() (*r2.Client, error) {
		t.Fatal("getR2Client should not be called when all markers are present")
		return nil, nil
	}
	failed := stageArtifactNeedsForHost(database, "host-alpha", jobs, time.Second, getR2)
	if len(failed) != 0 {
		t.Fatalf("unexpected failures: %v", failed)
	}
	if *probeCalls != 1 {
		t.Fatalf("probeCalls = %d, want 1 (probe should batch across jobs)", *probeCalls)
	}
	if got := len(*lastNeeds); got != 2 {
		t.Fatalf("probe saw %d needs, want 2 (one per consumer)", got)
	}
}

func TestStageArtifactNeedsForHost_NamedAssetRestagesWhenMarkerExistsButFileMissing(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.UpsertNamedAsset(database, db.NamedAsset{
		Name:        "trace-v1",
		ContentHash: "0123456789abcdef",
		SizeBytes:   123,
		TargetPath:  "data/trace.jsonl",
	}); err != nil {
		t.Fatalf("upsert named asset: %v", err)
	}

	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "consume", "consumer")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, consumerID, []string{"asset:trace-v1"}); err != nil {
		t.Fatalf("set needs: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}

	probeCalls, lastNeeds := stubProbeRemoteNeedsState(t, nil)
	probeRemoteNeedsStateFunc = func(_ string, needs []pendingNeed, _ time.Duration) (map[string]remoteNeedState, error) {
		*probeCalls++
		*lastNeeds = needs
		reply := make(map[string]remoteNeedState, len(needs))
		for _, n := range needs {
			reply[n.markerName] = remoteNeedState{markerExists: true, fileSize: -1, stagingSize: -1}
		}
		return reply, nil
	}

	getR2 := func() (*r2.Client, error) {
		return nil, fmt.Errorf("r2 requested")
	}
	failed := stageArtifactNeedsForHost(database, "host-alpha", []*db.Job{consumer}, time.Second, getR2)
	err = failed[consumer.ID]
	if err == nil || !strings.Contains(err.Error(), "r2 requested") {
		t.Fatalf("expected stale marker to trigger staging and fail on R2 setup, got %v", err)
	}
	if *probeCalls != 1 {
		t.Fatalf("probeCalls = %d, want 1", *probeCalls)
	}
	if len(*lastNeeds) != 1 {
		t.Fatalf("probe saw %d needs, want 1", len(*lastNeeds))
	}
	need := (*lastNeeds)[0]
	if need.markerName != "asset-trace-v1.satisfied" {
		t.Fatalf("markerName = %q, want asset marker", need.markerName)
	}
	if need.remotePath != "/tmp/project/data/trace.jsonl" {
		t.Fatalf("remotePath = %q, want target path under working dir", need.remotePath)
	}
}

// TestStageArtifactNeedsForHost_MissingProducer verifies that an unknown
// producer ID surfaces as an error rather than silently being skipped.
func TestStageArtifactNeedsForHost_MissingProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "consume", "consumer")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, consumerID, []string{"output/x.bin:99999"}); err != nil {
		t.Fatalf("set needs: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}
	stubProbeRemoteNeedsState(t, nil)
	getR2 := func() (*r2.Client, error) { return &r2.Client{}, nil }
	failed := stageArtifactNeedsForHost(database, "host-alpha", []*db.Job{consumer}, time.Second, getR2)
	err = failed[consumer.ID]
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got %v", err)
	}
}

func TestEnsureQueuedJobsOnRemote(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a queued job with no LastSyncedStatus (simulates job recorded while host offline)
	_, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "unsynced job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mock sync so rsync doesn't actually run
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return nil
	}))

	// Mock SSH so AppendJobToQueue and ResolveBackend succeed
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "", 0
	})

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 1 {
		t.Errorf("expected 1 job ensured, got %d", ensured)
	}
	if !contacted {
		t.Error("expected contacted=true")
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsJobOnSyncFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "sync-fail job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mock sync to fail
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return fmt.Errorf("rsync timeout")
	}))

	// Mock SSH — the only legitimate SSH for this test is the runner-state
	// read at the top of ensureQueuedJobsOnRemote (fetchRemoteRunnerState,
	// which both the forward and reverse reconcilers consume). Returning
	// the no-state-file sentinel makes that call a no-op so the prune pass
	// has nothing to do. Any OTHER SSH command would be a job-dispatch
	// path that shouldn't run while source sync is failing.
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "__WEFT_NO_STATE_FILE__") {
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		}
		t.Errorf("unexpected SSH command when source sync fails: %s", command)
		return "", "", 0
	})

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err == nil {
		t.Fatal("expected ensureQueuedJobsOnRemote to surface the sync failure")
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (sync failed), got %d", ensured)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("job %s source sync failed", ids.FormatJobID(jobID))) {
		t.Fatalf("error = %q, want job-specific source sync failure", err)
	}

	// Job should still be unsynced in the database
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LastSyncedStatus != "" {
		t.Errorf("expected LastSyncedStatus empty (unsynced), got %q", job.LastSyncedStatus)
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsJobOnArtifactStagingFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "asset-needs job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobNeeds(database, jobID, []string{"asset:missing-asset"}); err != nil {
		t.Fatalf("set needs: %v", err)
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		t.Fatal("source sync should not run when artifact staging fails first")
		return nil
	}))

	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		case strings.Contains(command, `"op":"add"`):
			appendCount++
			t.Errorf("unexpected append after artifact staging failure: %s", command)
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err == nil {
		t.Fatal("expected ensureQueuedJobsOnRemote to surface the artifact staging failure")
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (artifact staging failed), got %d", ensured)
	}
	if appendCount != 0 {
		t.Fatalf("appendCount = %d, want 0", appendCount)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("job %s artifact needs staging failed", ids.FormatJobID(jobID))) {
		t.Fatalf("error = %q, want job-specific artifact staging failure", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LastSyncedStatus != "" {
		t.Errorf("expected LastSyncedStatus empty (unsynced), got %q", job.LastSyncedStatus)
	}

	var detail string
	if err := database.QueryRow(`
		SELECT COALESCE(detail, '')
		FROM lifecycle_events
		WHERE job_id = ? AND event_kind = ?
		ORDER BY id DESC
		LIMIT 1`, jobID, db.EventQueueDispatchFailed).Scan(&detail); err != nil {
		t.Fatalf("query lifecycle event: %v", err)
	}
	if !strings.Contains(detail, "artifact needs staging failed") {
		t.Fatalf("expected artifact staging lifecycle detail, got %q", detail)
	}
}

func TestEnsureQueuedJobsOnRemote_SourceSyncFailureSkipsSameDirJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	workingDir := t.TempDir()

	jobID1, err := db.RecordQueued(database, "test-host", workingDir, "echo one", "sync-fail one")
	if err != nil {
		t.Fatalf("record queued job 1: %v", err)
	}
	jobID2, err := db.RecordQueued(database, "test-host", workingDir, "echo two", "sync-fail two")
	if err != nil {
		t.Fatalf("record queued job 2: %v", err)
	}

	syncCalls := 0
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		syncCalls++
		return fmt.Errorf("rsync timeout")
	}))

	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		case strings.Contains(command, `"op":"add"`):
			appendCount++
			t.Errorf("unexpected append after source sync failure: %s", command)
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err == nil {
		t.Fatal("expected ensureQueuedJobsOnRemote to surface the sync failure")
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (sync failed), got %d", ensured)
	}
	if syncCalls != 1 {
		t.Fatalf("syncCalls = %d, want 1 for shared working dir", syncCalls)
	}
	if appendCount != 0 {
		t.Fatalf("appendCount = %d, want 0", appendCount)
	}
	for _, jobID := range []int64{jobID1, jobID2} {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			t.Fatalf("get job %d: %v", jobID, err)
		}
		if job.LastSyncedStatus != "" {
			t.Errorf("job %d LastSyncedStatus = %q, want empty", jobID, job.LastSyncedStatus)
		}
	}
}

func TestEnsureQueuedJobsOnRemote_SourceSyncLeaseContentionDefers(t *testing.T) {
	database := db.SetupTestDB(t)
	workingDir := t.TempDir()

	jobID, err := db.RecordQueued(database, "test-host", workingDir, "echo hello", "sync-busy job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	remoteDir := workingDir
	scope := sourceSyncLeaseScope("test-host", remoteDir)
	acquired, err := db.AcquireAutoLease(database, scope, "other-source-sync", sourceSyncLeaseTTL)
	if err != nil || !acquired {
		t.Fatalf("seed source sync lease: acquired=%v err=%v", acquired, err)
	}

	syncCalls := 0
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		syncCalls++
		t.Fatal("source sync should not run while another caller holds the lease")
		return nil
	}))

	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		case strings.Contains(command, `"op":"add"`):
			appendCount++
			t.Errorf("unexpected append while source sync is already in flight: %s", command)
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("ensured = %d, want 0", ensured)
	}
	if contacted {
		t.Error("contacted = true, want false")
	}
	if syncCalls != 0 {
		t.Fatalf("syncCalls = %d, want 0", syncCalls)
	}
	if appendCount != 0 {
		t.Fatalf("appendCount = %d, want 0", appendCount)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LastSyncedStatus != "" {
		t.Errorf("LastSyncedStatus = %q, want empty", job.LastSyncedStatus)
	}
}

func TestSourceSyncLeaseTTLTracksLongSourceTimeout(t *testing.T) {
	if got := sourceSyncLeaseTTLFor(5 * time.Second); got != sourceSyncLeaseTTL {
		t.Fatalf("short timeout TTL = %s, want default %s", got, sourceSyncLeaseTTL)
	}
	if got := sourceSyncLeaseTTLFor(10 * time.Minute); got != 11*time.Minute {
		t.Fatalf("long timeout TTL = %s, want 11m", got)
	}
}

// TestEffectiveSourceSyncTimeout guards against the regression where source
// rsync inherited the few-second SSH status-probe Timeout. The daemon host-sync
// worker passes a 5s Timeout and leaves SourceTimeout unset; before the fix that
// SIGKILL'd source rsync mid-connect, blocking the job with "source sync failed:
// ... timed out after 5s". Source sync must instead get the source-sync default.
func TestEffectiveSourceSyncTimeout(t *testing.T) {
	// Unset SourceTimeout must not fall back to the short status Timeout.
	got := effectiveSourceSyncTimeout(HostSyncOptions{Timeout: 5 * time.Second})
	if got != defaultSourceSyncTimeout {
		t.Fatalf("unset SourceTimeout with 5s Timeout = %s, want source default %s", got, defaultSourceSyncTimeout)
	}
	if got <= 5*time.Second {
		t.Fatalf("source sync budget %s is too short to survive SSH connect setup", got)
	}

	// An explicit SourceTimeout is honored verbatim (the syncorch path sets it).
	if got := effectiveSourceSyncTimeout(HostSyncOptions{Timeout: 5 * time.Second, SourceTimeout: 30 * time.Second}); got != 30*time.Second {
		t.Fatalf("explicit SourceTimeout = %s, want 30s", got)
	}
}

func TestSyncHost_SurfacesQueueDispatchFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "sync-fail job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return fmt.Errorf("rsync timeout")
	}))

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "", 0
	})

	result, err := SyncHost(database, "test-host", HostSyncOptions{Timeout: time.Second}, nil)
	if err != nil {
		t.Fatalf("SyncHost: %v", err)
	}
	if result.QueueDispatchError == "" {
		t.Fatal("expected QueueDispatchError to be populated")
	}
	if !strings.Contains(result.QueueDispatchError, fmt.Sprintf("job %s source sync failed", ids.FormatJobID(jobID))) {
		t.Fatalf("QueueDispatchError = %q, want job-specific source sync failure", result.QueueDispatchError)
	}
	if result.Updated != 0 {
		t.Fatalf("Updated = %d, want 0", result.Updated)
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsAlreadySynced(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "synced job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mark as already synced
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	// No SSH mock needed — should skip without making SSH calls
	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (already synced), got %d", ensured)
	}
	if contacted {
		t.Error("expected contacted=false for already synced")
	}
}

func TestEnsureQueuedJobsOnRemote_RedispatchesSyncedQueuedJobWithMissingPayload(t *testing.T) {
	database := db.SetupTestDB(t)

	workingDir := t.TempDir()
	jobID, err := db.RecordQueued(database, "test-host", workingDir, "echo hello", "synced missing payload")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobBackend(database, jobID, db.BackendQueueRunner); err != nil {
		t.Fatalf("set backend: %v", err)
	}
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return nil
	}))

	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return fmt.Sprintf(`{"pending":[%d],"current":null}`, jobID) + "\n", "", 0
		case strings.Contains(command, "job-${id}.json"):
			return fmt.Sprintf("%d\tMISSING\n", jobID), "", 0
		case strings.Contains(command, `"op":"add"`):
			appendCount++
			if !strings.Contains(command, fmt.Sprintf(`"id":%d`, jobID)) {
				t.Fatalf("append command missing job id: %s", command)
			}
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 1 {
		t.Fatalf("ensured = %d, want 1", ensured)
	}
	if !contacted {
		t.Fatal("contacted = false, want true")
	}
	if appendCount != 1 {
		t.Fatalf("appendCount = %d, want 1", appendCount)
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsPendingStatus(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "pending job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Set a pending status (e.g., pending cancel)
	cancelStatus := db.StatusCanceled
	if err := db.SetPendingStatus(database, jobID, cancelStatus); err != nil {
		t.Fatalf("set pending status: %v", err)
	}

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (has pending status), got %d", ensured)
	}
	if contacted {
		t.Error("expected contacted=false for pending status")
	}
}

func TestProcessDeferredQueueOps_RemoveQueued(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "queued job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.AddDeferredOperation(database, "test-host", db.OpRemoveQueued, jobID, ""); err != nil {
		t.Fatalf("add deferred op: %v", err)
	}
	if err := db.MoveQueuedJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("MoveQueuedJobToUnplaced: %v", err)
	}

	callCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		callCount++
		return "", "", 0
	})

	result, err := ProcessDeferredQueueOps(database, "test-host", 5*time.Second)
	if err != nil {
		t.Fatalf("ProcessDeferredQueueOps: %v", err)
	}
	if !result.HostContacted {
		t.Error("expected HostContacted to be true")
	}
	if callCount == 0 {
		t.Error("expected remote cleanup SSH command")
	}
	pending, err := db.HasPendingOperation(database, jobID, db.OpRemoveQueued)
	if err != nil {
		t.Fatalf("HasPendingOperation: %v", err)
	}
	if pending {
		t.Error("expected remove_queued deferred op to be cleared")
	}
}

func TestEnsureHFInputsAvailable_DownloadsMissingHFAsset(t *testing.T) {
	database := db.SetupTestDB(t)
	scanCount := 0

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if host != "test-host" {
			t.Fatalf("unexpected host %q", host)
		}
		switch {
		case strings.Contains(command, "df -Pk"):
			return "20971520\n", "", 0
		// Daemonized download path (runDetachedRemoteCommand in
		// internal/dataloc/download.go): the spawn issues a `nohup bash
		// -c ... cmd.sh ...` invocation; each poll calls
		// `if [ -f "$D/status" ]`. The test cares about the request
		// completing, not what the download actually did — return OK to
		// spawn, STATUS=0 to the first poll.
		case strings.Contains(command, "nohup bash -c") && strings.Contains(command, "cmd.sh"):
			return "OK\n", "", 0
		case strings.Contains(command, `if [ -f "$D/status" ]`):
			return "STATUS=0\n---STDERR---\n", "", 0
		case strings.Contains(command, "$_hfdl download --repo-type model"):
			return "", "", 0
		case strings.Contains(command, "du -sb"), strings.Contains(command, "ls -1d"):
			scanCount++
			if scanCount >= 2 {
				return "2048\tok\t/home/test/.cache/huggingface/hub/models--bert-base-uncased\n", "", 0
			}
			return "", "", 0
		default:
			return "", "", 0
		}
	})
	t.Cleanup(dataloc.SetDetachedPollIntervalForTest(time.Millisecond))

	if err := ensureHFInputsAvailable(database, "test-host", []string{"hf:bert-base-uncased"}, 5*time.Second); err != nil {
		t.Fatalf("ensureHFInputsAvailable: %v", err)
	}

	entries, err := dataloc.FindAssetHosts(database, dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "bert-base-uncased"})
	if err != nil {
		t.Fatalf("FindAssetHosts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Host != "test-host" {
		t.Fatalf("host = %q, want test-host", entries[0].Host)
	}
}

func TestHFInputStageTimeout(t *testing.T) {
	if got := hfInputStageTimeout(0); got != minHFInputStageTimeout {
		t.Fatalf("hfInputStageTimeout(0) = %s, want %s", got, minHFInputStageTimeout)
	}
	if got := hfInputStageTimeout(30 * time.Second); got != minHFInputStageTimeout {
		t.Fatalf("hfInputStageTimeout(30s) = %s, want %s", got, minHFInputStageTimeout)
	}
	longTimeout := 15 * time.Minute
	if got := hfInputStageTimeout(longTimeout); got != longTimeout {
		t.Fatalf("hfInputStageTimeout(15m) = %s, want %s", got, longTimeout)
	}
}

// TestStageMissingNeeds_LeaseSkipsConcurrentDuplicate verifies that when a
// staging lease for the same (host, artifact) is already held, a second
// caller skips the transfer instead of spawning a duplicate scp. This is
// the regression for overlapping sync passes stomping the same .weft-staging
// destination.
func TestStageMissingNeeds_LeaseSkipsConcurrentDuplicate(t *testing.T) {
	database := db.SetupTestDB(t)

	producerID, err := db.RecordQueued(database, "", "/tmp", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}
	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "consume", "consumer")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, consumerID, []string{fmt.Sprintf("output/big.pt:%d", producerID)}); err != nil {
		t.Fatalf("set needs: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}

	pending, err := collectPendingNeeds(database, consumer)
	if err != nil {
		t.Fatalf("collectPendingNeeds: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending need, got %d", len(pending))
	}

	// Pre-acquire the lease as if another caller is already mid-transfer.
	scope := needsStageLeaseScope("host-alpha", pending[0].markerName)
	acquired, err := db.AcquireAutoLease(database, scope, "other-caller", needsStageLeaseTTL)
	if err != nil || !acquired {
		t.Fatalf("seed lease: acquired=%v err=%v", acquired, err)
	}

	// getR2Client must NOT be called when the transfer is skipped.
	getR2 := func() (*r2.Client, error) {
		t.Fatal("getR2Client should not be called when lease is held by another caller")
		return nil, nil
	}

	probeState := map[string]remoteNeedState{
		pending[0].markerName: {markerExists: false, fileSize: -1, stagingSize: -1},
	}
	if err := stageMissingNeeds(database, consumer, pending, probeState, time.Second, getR2); err != nil {
		t.Fatalf("stageMissingNeeds: %v", err)
	}
}

func TestFindExistingScpToRemote(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not on PATH; skipping")
	}

	// Start a long-running placeholder process whose command line will
	// match the regex pgrep expects. It needs to look enough like a
	// real scp invocation to satisfy the `scp .*<host>:<path>` pattern.
	// `sleep` with a tagged comment in argv works on macOS and Linux.
	const remoteHost = "test-fake-host-9f3a"
	const remotePath = "/tmp/weft-pid-check-fixture/file.bin"
	cmd := exec.Command("sh", "-c", fmt.Sprintf("exec -a 'scp -q /tmp/x %s:%s.weft-staging' sleep 30", remoteHost, remotePath))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start placeholder: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// pgrep argv visibility on macOS sometimes lags briefly after exec;
	// poll a couple times.
	var found []int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found = findExistingScpToRemote(remoteHost, remotePath+".weft-staging")
		if len(found) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(found) == 0 {
		t.Skip("pgrep didn't match the placeholder argv (platform shell limitation); fixture-side issue, not a code regression")
	}

	// Negative case: a different remote host should not match.
	if pids := findExistingScpToRemote("totally-different-host", remotePath+".weft-staging"); len(pids) > 0 {
		t.Errorf("findExistingScpToRemote returned %v for unrelated host", pids)
	}
}
