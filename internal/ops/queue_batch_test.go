package ops

import (
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
	reason := "source_provenance_mismatch: expected=abc123, marker=def456"
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
