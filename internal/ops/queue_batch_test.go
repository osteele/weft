package ops

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestParseBatchSourceExecution(t *testing.T) {
	parts := strings.Split("JOB|42|RUNNING|0|pinned_inventory_manifest|source_manifest_v2|submitted|verified|verified|123|agent-1|2", "|")
	got := parseBatchSourceExecution(parts, 4)
	if got == nil || got.DispatchMode != "pinned_inventory_manifest" || got.VerifiedAt != 123 || got.AgentVersion != "agent-1" || got.RootCount != 2 {
		t.Fatalf("source execution = %+v", got)
	}
}

func TestParseQueueBatchStatusSanitizesRemoteFailureReason(t *testing.T) {
	statuses := parseQueueBatchStatusOutput("JOB|42|COMPLETED|1|123|7|legacy prose|shifted|source|fields|that|look|valid|123|agent|1\n", 1)
	if got := statuses[42].FailureReason; got != db.FailureReasonError {
		t.Fatalf("failure reason = %q, want %q", got, db.FailureReasonError)
	}
	if statuses[42].Source != nil {
		t.Fatalf("untrusted reason populated shifted source metadata: %+v", statuses[42].Source)
	}
	statuses = parseQueueBatchStatusOutput("JOB|43|PREFLIGHT_REJECTED|123|legacy prose|shifted-agent-version\n", 1)
	if got := statuses[43].FailureReason; got != db.FailureReasonError {
		t.Fatalf("preflight failure reason = %q, want %q", got, db.FailureReasonError)
	}
	if statuses[43].AgentVersion != "" {
		t.Fatalf("untrusted reason populated shifted agent version %q", statuses[43].AgentVersion)
	}
}

func TestParseQueueBatchStatusRequiresAttemptForUnresolvedObservation(t *testing.T) {
	statuses := parseQueueBatchStatusOutput("JOB|42|UNRESOLVED_CANDIDATE|1042\n", 1)
	status, ok := statuses[42]
	if !ok || status.State != queueStateUnresolvedCandidate || status.RunID != 1042 {
		t.Fatalf("parsed status = %+v, present=%v", status, ok)
	}
	for _, malformed := range []string{
		"JOB|42|UNRESOLVED_CANDIDATE\n",
		"JOB|42|UNRESOLVED_CANDIDATE|0\n",
		"JOB|42|UNRESOLVED_CANDIDATE|not-a-run\n",
	} {
		if got := parseQueueBatchStatusOutput(malformed, 1); len(got) != 0 {
			t.Fatalf("malformed unresolved status %q parsed as %+v", malformed, got)
		}
	}
}

func TestBatchSyncPersistsAttemptSourceVerification(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "source verification")
	if err != nil {
		t.Fatal(err)
	}
	meta := &db.JobMetadata{Source: &db.JobSourceMetadata{Pin: &db.JobSourcePinMetadata{Hash: "manifest-a", Roots: []db.JobSourcePinRootMetadata{{Hash: "root-a"}}}}}
	if err := db.SetJobMetadata(database, jobID, meta); err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetJobByID(database, jobID)
	statuses := map[int64]queueBatchStatus{jobID: {State: queueStateQueued, Source: &db.JobSourceExecutionMetadata{
		DispatchMode: "pinned_inventory_manifest", IdentityKind: db.SourceIdentityManifestV2, DispatchedSHA256: "manifest-a", VerifiedSHA256: "manifest-a", Verification: db.SourceVerificationVerified,
	}}}
	if _, err := applyBatchStatuses(database, []int64{jobID}, map[int64]*db.Job{jobID: job}, statuses, time.Second); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetJobByID(database, jobID)
	if got.Metadata == nil || got.Metadata.Source == nil || got.Metadata.Source.Execution == nil || got.Metadata.Source.Execution.Verification != db.SourceVerificationVerified {
		t.Fatalf("metadata = %+v", got.Metadata)
	}
	if got.Metadata.Source.Execution.SubmittedIdentityKind != db.SourceIdentityManifestV2 {
		t.Fatalf("execution = %+v", got.Metadata.Source.Execution)
	}
}

// TestBatchSyncDeadReportDoesNotKillRecentlyQueuedJob validates the fix for the
// race condition where the TUI's batch sync marks recently-queued jobs as dead.
//
// Scenario: A job is queued and appended to the remote commands file
// (last_synced_status=queued), but the queue runner hasn't processed the command
// yet. The agent reports DEAD because the job isn't in the runner's state.
// The batch sync must NOT mark this job as failed.
func TestBatchSyncDeadReportDoesNotKillRecentlyQueuedJob(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "recently queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStateDead},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updates, got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusQueued {
		t.Fatalf("expected status queued, got %s", result.Status)
	}
}

// TestBatchSyncDeadReportKillsRunningJob validates that the batch sync DOES mark
// a running job as dead when the agent reports DEAD (process disappeared).
func TestBatchSyncDeadReportKillsRunningJob(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "running job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStateDead},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 update, got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusFailed {
		t.Fatalf("expected status failed, got %s", result.Status)
	}
}

// TestBatchSyncDeadProtectsQueuedJobWithSyncedStatus validates that a queued job
// whose last_synced_status is "queued" (set after first sync) is protected from
// being marked dead by batch sync. This is the normal case for synced jobs.
func TestBatchSyncDeadProtectsQueuedJobWithSyncedStatus(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "new job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	// Simulate the job having been synced to the remote queue
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStateDead},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updates (job should be protected), got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusQueued {
		t.Fatalf("expected status queued (protected), got %s", result.Status)
	}
}

// TestBatchSyncRunningTransitionsQueuedJob validates that a queued job is
// correctly marked running when the agent reports it as RUNNING.
func TestBatchSyncRunningTransitionsQueuedJob(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "queue to run")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	mock := mockQueueRemote{metadata: "start_time=1700000000\n"}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStateRunning, GPUDevices: "0,1"},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 update, got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusRunning {
		t.Fatalf("expected status running, got %s", result.Status)
	}
}

// TestBatchSyncPreflightRejectedRecordsReasonAndDispatchBlock validates
// Layer A+B: a PREFLIGHT_REJECTED batch-status report closes the attempt
// with failure_reason populated AND inserts an EventQueueDispatchFailed
// lifecycle event so explain.LatestInventoryDispatchBlock surfaces the
// reason via `weft job diagnose`. Crucially, no RecordJobCompletion runs,
// so the attempt does not get a misleading exit_code=1 / duration=0s.
func TestBatchSyncPreflightRejectedRecordsReasonAndDispatchBlock(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "rejected job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	reason := db.FailureReasonSourceProvenanceMismatch
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStatePreflightRejected, FailureReason: reason, Mtime: time.Now().Unix()},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 update, got %d", updated)
	}

	// failure_reason must be persisted on the attempt for the display layer
	// to pick it up.
	result, _ := db.GetJobByID(database, jobID)
	if result.FailureReason != reason {
		t.Fatalf("failure_reason = %q, want %q", result.FailureReason, reason)
	}

	// A dispatch-failed lifecycle event must be present so diagnose surfaces
	// the reason via LatestInventoryDispatchBlock.
	events, lerr := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
		Kind: db.EventQueueDispatchFailed,
	})
	if lerr != nil {
		t.Fatalf("ListLifecycleEvents: %v", lerr)
	}
	var found bool
	for _, ev := range events {
		if ev.JobID == jobID && ev.Detail == reason {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected EventQueueDispatchFailed event for job %d with detail %q; events=%+v", jobID, reason, events)
	}
}

// TestBatchSyncPreflightRejectedRecordsFailureDetail validates that the
// runner's rejection detail (e.g. "pinned_source_fetch_failed: mkdir
// .../rules: permission denied") is persisted on the dispatch-failed
// lifecycle event, not just the classified failure_reason — `weft info`
// renders the cause from this event next to the Reason line.
func TestBatchSyncPreflightRejectedRecordsFailureDetail(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "rejected job with detail")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	reason := db.FailureReasonPinnedSourceFetchFailed
	detail := "pinned_source_fetch_failed: mkdir /srv/rules: permission denied"
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStatePreflightRejected, RunID: 1, FailureReason: reason, FailureDetail: detail, Mtime: time.Now().Unix()},
	}

	if _, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second); err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}

	got, err := db.LatestJobDispatchFailureDetail(database, jobID, reason)
	if err != nil {
		t.Fatalf("LatestJobDispatchFailureDetail: %v", err)
	}
	if got != detail {
		t.Fatalf("dispatch failure detail = %q, want %q", got, detail)
	}
}

// TestBatchSyncCompletedWithFailureReasonAlsoRecordsDispatchBlock validates
// that the existing exit-code path also records a dispatch-failed lifecycle
// event when a failure_reason is set. This keeps the diagnose surface
// consistent regardless of whether the agent reported COMPLETED or
// PREFLIGHT_REJECTED.
func TestBatchSyncCompletedWithFailureReasonAlsoRecordsDispatchBlock(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "completed-with-reason")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	exit := 1
	reason := "oom"
	mock := mockQueueRemote{metadata: "start_time=1700000000\nend_time=1700000010\n"}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	statuses := map[int64]queueBatchStatus{
		jobID: {ExitCode: &exit, Mtime: 1700000020, FailureReason: reason},
	}
	if _, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second); err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}

	events, lerr := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
		Kind: db.EventQueueDispatchFailed,
	})
	if lerr != nil {
		t.Fatalf("ListLifecycleEvents: %v", lerr)
	}
	var found bool
	for _, ev := range events {
		if ev.JobID == jobID && ev.Detail == reason {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected EventQueueDispatchFailed event for job %d with detail %q", jobID, reason)
	}
}

// TestBatchSyncCompletedBackfillsStartTimeFromCompletionRecord validates the
// second evidence source for start_time: when a completion is recorded on a
// tick where the metadata read yields nothing (the job was never observed
// running and the meta file is unreadable), the completion record's
// start_time is used, so the attempt does not close with end_time but NULL
// start_time (live incident: wj3871–3873).
func TestBatchSyncCompletedBackfillsStartTimeFromCompletionRecord(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "completed-unseen")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	if job.StartTime != 0 {
		t.Fatalf("precondition: start_time = %d, want 0", job.StartTime)
	}
	jobByID := map[int64]*db.Job{jobID: job}
	exit := 0
	mock := mockQueueRemote{
		metadata:         "",
		completionRecord: `{"exit_code":0,"start_time":1700000001,"end_time":1700000011}`,
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	statuses := map[int64]queueBatchStatus{
		jobID: {ExitCode: &exit, Mtime: 1700000011},
	}
	if _, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second); err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusCompleted {
		t.Fatalf("status = %s, want completed", result.Status)
	}
	if result.StartTime != 1700000001 {
		t.Fatalf("start_time = %d, want 1700000001 (backfilled from completion record)", result.StartTime)
	}
}

func TestBatchSyncCompletedIgnoresStaleRunID(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "stale completion")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark first attempt running: %v", err)
	}
	exit := 1
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exit, 1700000010); err != nil {
		t.Fatalf("close first attempt: %v", err)
	}
	if err := db.RequeueFreshAttemptByTarget(database, jobID, "batch-host", nil); err != nil {
		t.Fatalf("fresh retry attempt: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LatestRunID == nil {
		t.Fatal("latest run id is nil")
	}
	freshRunID := *job.LatestRunID
	if freshRunID <= 1 {
		t.Fatalf("fresh run id = %d, want a retry attempt id", freshRunID)
	}
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{
		jobID: {ExitCode: &exit, Mtime: 1700000020, RunID: freshRunID - 1},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected stale completion to be ignored, got %d updates", updated)
	}

	result, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get result: %v", err)
	}
	if result.LatestRunID == nil || *result.LatestRunID != freshRunID {
		t.Fatalf("latest run id = %v, want %d", result.LatestRunID, freshRunID)
	}
	if result.Status != db.StatusQueued {
		t.Fatalf("status = %s, want queued", result.Status)
	}
	if result.ExitCode != nil {
		t.Fatalf("exit code = %v, want nil on fresh retry", *result.ExitCode)
	}
}

// TestBatchSyncSkipsUnknownJobs validates that jobs not in the agent's response
// are silently skipped (no status change).
func TestBatchSyncSkipsUnknownJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "unknown job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{} // agent doesn't know about this job

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updates, got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusQueued {
		t.Fatalf("expected status queued, got %s", result.Status)
	}
}

func TestApplyBatchStatusesR2FallbackWhenMarkerMissingAfterGracePeriod(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "studio", "/tmp", "echo test", "r2 missing marker")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	origSync := syncJobStatusFromR2ForBatch
	t.Cleanup(func() { syncJobStatusFromR2ForBatch = origSync })
	syncJobStatusFromR2ForBatch = func(_ *sql.DB, _ *db.Job) (SyncResult, error) {
		return SyncResult{}, fmt.Errorf("R2 completion not found")
	}

	exitCode := 0
	finishedAt := int64(1700000000)
	statuses := map[int64]queueBatchStatus{
		jobID: {RunID: *job.LatestRunID, ExitCode: &exitCode, Mtime: finishedAt, FromR2: true},
	}

	// 1. When within the grace period (30s after finish), sync is skipped to wait for R2.
	nowRecent := time.Unix(finishedAt+30, 0)
	updated, err := applyBatchStatusesAt(database, []int64{jobID}, map[int64]*db.Job{jobID: job}, statuses, time.Second, effectiveQueueUnknownAfter(0), nowRecent)
	if err != nil {
		t.Fatalf("applyBatchStatusesAt: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updates within grace period, got %d", updated)
	}
	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusQueued {
		t.Fatalf("expected status queued, got %s", result.Status)
	}

	// 2. When after the grace period (3 minutes after finish), sync falls back to runner state.
	nowExpired := time.Unix(finishedAt+180, 0)
	updated, err = applyBatchStatusesAt(database, []int64{jobID}, map[int64]*db.Job{jobID: job}, statuses, time.Second, effectiveQueueUnknownAfter(0), nowExpired)
	if err != nil {
		t.Fatalf("applyBatchStatusesAt: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 update after grace period, got %d", updated)
	}
	result, _ = db.GetJobByID(database, jobID)
	if result.Status != db.StatusCompleted || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("expected status completed with exit 0, got status=%s exit=%v", result.Status, result.ExitCode)
	}
	if result.EndTime == nil || *result.EndTime != finishedAt {
		t.Fatalf("expected end time %d, got %v", finishedAt, result.EndTime)
	}
}

func lifecycleEventsForJob(t *testing.T, database *sql.DB, jobID int64, kind string) []db.LifecycleEvent {
	t.Helper()
	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{Kind: kind, JobID: jobID})
	if err != nil {
		t.Fatalf("list %s events: %v", kind, err)
	}
	return events
}

// An older agent publishes a job-only finished entry with no run id. On a job
// with a single attempt that entry admits exactly one reading, so the
// completion settles rather than leaving the job reporting `running` forever.
func TestApplyBatchStatusesSettlesUnfencedCompletionOnSingleAttemptJob(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "studio", "/tmp", "echo test", "unfenced completion")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	// The attempt must have been queued before the host says it finished;
	// a completion that predates its attempt describes some earlier run.
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, 1700000000-600, jobID); err != nil {
		t.Fatalf("backdate attempt: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	attempts, err := db.ListAttempts(database, jobID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %d, %v; want exactly 1", len(attempts), err)
	}

	origSync := syncJobStatusFromR2ForBatch
	t.Cleanup(func() { syncJobStatusFromR2ForBatch = origSync })
	syncJobStatusFromR2ForBatch = func(_ *sql.DB, _ *db.Job) (SyncResult, error) {
		return SyncResult{}, fmt.Errorf("R2 completion not found")
	}

	exitCode := 3
	finishedAt := int64(1700000000)
	statuses := map[int64]queueBatchStatus{
		// RunID 0: the older agent's job-only entry.
		jobID: {RunID: 0, ExitCode: &exitCode, Mtime: finishedAt, FromR2: true},
	}

	updated, err := applyBatchStatusesAt(database, []int64{jobID}, map[int64]*db.Job{jobID: job}, statuses, time.Second, effectiveQueueUnknownAfter(0), time.Unix(finishedAt+180, 0))
	if err != nil {
		t.Fatalf("applyBatchStatusesAt: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}

	result, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get result: %v", err)
	}
	if result.Status != db.StatusFailed || result.ExitCode == nil || *result.ExitCode != exitCode {
		t.Fatalf("status=%s exit=%v, want failed with exit %d", result.Status, result.ExitCode, exitCode)
	}
	if result.EndTime == nil || *result.EndTime != finishedAt {
		t.Fatalf("end time = %v, want %d", result.EndTime, finishedAt)
	}
	if events := lifecycleEventsForJob(t, database, jobID, db.EventQueueCompletionUnattributable); len(events) != 0 {
		t.Fatalf("unattributable events = %d, want 0 for an unambiguous job", len(events))
	}
}

// A single-attempt job is only unambiguous if the entry could have come from
// that attempt. A job-only entry whose finish time predates the attempt's
// queueing describes an earlier run of the same job id, so attributing it
// would overwrite a live attempt with a stale outcome.
func TestApplyBatchStatusesRefusesUnfencedCompletionOlderThanItsAttempt(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "studio", "/tmp", "echo test", "stale unfenced completion")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	finishedAt := int64(1700000000)
	// The one live attempt was queued after the host-reported finish.
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, finishedAt+600, jobID); err != nil {
		t.Fatalf("set attempt queued_at: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	origSync := syncJobStatusFromR2ForBatch
	t.Cleanup(func() { syncJobStatusFromR2ForBatch = origSync })
	syncJobStatusFromR2ForBatch = func(_ *sql.DB, _ *db.Job) (SyncResult, error) {
		return SyncResult{}, fmt.Errorf("R2 completion not found")
	}

	exitCode := 0
	statuses := map[int64]queueBatchStatus{
		jobID: {RunID: 0, ExitCode: &exitCode, Mtime: finishedAt, FromR2: true},
	}

	updated, err := applyBatchStatusesAt(database, []int64{jobID}, map[int64]*db.Job{jobID: job}, statuses, time.Second, effectiveQueueUnknownAfter(0), time.Unix(finishedAt+1800, 0))
	if err != nil {
		t.Fatalf("applyBatchStatusesAt: %v", err)
	}
	if updated != 0 {
		t.Fatalf("updated = %d, want 0: the completion predates the only attempt", updated)
	}
	result, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get result: %v", err)
	}
	if result.Status != db.StatusQueued || result.ExitCode != nil || result.EndTime != nil {
		t.Fatalf("status=%s exit=%v end=%v, want the queued attempt untouched", result.Status, result.ExitCode, result.EndTime)
	}
	events := lifecycleEventsForJob(t, database, jobID, db.EventQueueCompletionUnattributable)
	if len(events) != 1 {
		t.Fatalf("unattributable events = %d, want 1", len(events))
	}
	if !strings.Contains(events[0].Detail, "predates") {
		t.Fatalf("detail = %q, want the stale-finish reason", events[0].Detail)
	}
}

// Same job-only entry, but the job has been retried, so the completion could
// describe either attempt. Settling could close an attempt still running on
// the host, so weft does not — and says so where a user can see it.
func TestApplyBatchStatusesRefusesUnfencedCompletionOnMultiAttemptJob(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "studio", "/tmp", "echo test", "ambiguous completion")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	firstExit := 1
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &firstExit, 1699999000); err != nil {
		t.Fatalf("close first attempt: %v", err)
	}
	if err := db.RequeueFreshAttemptByTarget(database, jobID, "studio", nil); err != nil {
		t.Fatalf("fresh retry attempt: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	attempts, err := db.ListAttempts(database, jobID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts = %d, %v; want 2", len(attempts), err)
	}

	origSync := syncJobStatusFromR2ForBatch
	t.Cleanup(func() { syncJobStatusFromR2ForBatch = origSync })
	syncJobStatusFromR2ForBatch = func(_ *sql.DB, _ *db.Job) (SyncResult, error) {
		return SyncResult{}, fmt.Errorf("R2 completion not found")
	}

	exitCode := 0
	finishedAt := int64(1700000000)
	statuses := map[int64]queueBatchStatus{
		jobID: {RunID: 0, ExitCode: &exitCode, Mtime: finishedAt, FromR2: true},
	}

	updated, err := applyBatchStatusesAt(database, []int64{jobID}, map[int64]*db.Job{jobID: job}, statuses, time.Second, effectiveQueueUnknownAfter(0), time.Unix(finishedAt+180, 0))
	if err != nil {
		t.Fatalf("applyBatchStatusesAt: %v", err)
	}
	if updated != 0 {
		t.Fatalf("updated = %d, want 0 for an unattributable completion", updated)
	}
	result, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get result: %v", err)
	}
	if db.IsTerminalStatus(result.Status) {
		t.Fatalf("status = %s, want a non-terminal status: the attempt was never identified", result.Status)
	}

	events := lifecycleEventsForJob(t, database, jobID, db.EventQueueCompletionUnattributable)
	if len(events) != 1 {
		t.Fatalf("unattributable events = %d, want 1", len(events))
	}
	if !strings.Contains(events[0].Detail, "cannot be attributed to an attempt") {
		t.Fatalf("detail = %q, want the naming of the condition", events[0].Detail)
	}
	if !strings.Contains(events[0].Detail, "2 attempts") {
		t.Fatalf("detail = %q, want the ambiguity reason", events[0].Detail)
	}
}

// wb181: a 47-minute gap between a host-side END and the recorded end_time
// could not be attributed afterwards because nothing recorded when weft first
// saw the host's terminal state. This event is that timestamp; the gap to
// end_time is the settlement latency.
func TestApplyBatchStatusesRecordsFirstHostReportedFinishOnce(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "studio", "/tmp", "echo test", "finish observation")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	origSync := syncJobStatusFromR2ForBatch
	t.Cleanup(func() { syncJobStatusFromR2ForBatch = origSync })
	syncJobStatusFromR2ForBatch = func(_ *sql.DB, _ *db.Job) (SyncResult, error) {
		return SyncResult{}, fmt.Errorf("R2 completion not found")
	}

	exitCode := 0
	finishedAt := int64(1700000000)
	statuses := map[int64]queueBatchStatus{
		jobID: {RunID: *job.LatestRunID, ExitCode: &exitCode, Mtime: finishedAt, FromR2: true},
	}

	// Two passes inside the R2 grace period: the job stays unsettled, so the
	// observation must not be re-recorded on every pass.
	for pass := range 2 {
		if _, err := applyBatchStatusesAt(database, []int64{jobID}, map[int64]*db.Job{jobID: job}, statuses, time.Second, effectiveQueueUnknownAfter(0), time.Unix(finishedAt+30, 0)); err != nil {
			t.Fatalf("pass %d: applyBatchStatusesAt: %v", pass, err)
		}
	}

	events := lifecycleEventsForJob(t, database, jobID, db.EventQueueCompletionObserved)
	if len(events) != 1 {
		t.Fatalf("observation events = %d, want 1", len(events))
	}
	wantFinish := time.Unix(finishedAt, 0).UTC().Format(time.RFC3339)
	if !strings.Contains(events[0].Detail, wantFinish) {
		t.Fatalf("detail = %q, want the host-reported finish time %s", events[0].Detail, wantFinish)
	}
	if !strings.Contains(events[0].Detail, fmt.Sprintf("attempt %d", *job.LatestRunID)) {
		t.Fatalf("detail = %q, want the attempt it describes", events[0].Detail)
	}
}
