package db

import (
	"testing"
	"time"
)

func TestCountJobFailedAttempts(t *testing.T) {
	database := SetupTestDB(t)

	// Job with no attempts -> 0
	insertTestJob(t, database, 100, "echo a", "/tmp", StatusQueued)
	if n, err := CountJobFailedAttempts(database, 100); err != nil || n != 0 {
		t.Fatalf("queued-only job: n=%d err=%v, want 0, nil", n, err)
	}

	// Job with one completed attempt, exit 0 -> 0
	insertTestJob(t, database, 101, "echo b", "/tmp", StatusCompleted, withExitCode(0))
	if n, err := CountJobFailedAttempts(database, 101); err != nil || n != 0 {
		t.Fatalf("clean completion: n=%d err=%v, want 0, nil", n, err)
	}

	// Job with one completed attempt, exit 1 -> 1 (legacy failure shape:
	// runner marks attempt completed with non-zero exit, the dispatcher
	// must treat this as a failure signal for R2 escalation).
	insertTestJob(t, database, 102, "echo c", "/tmp", StatusCompleted, withExitCode(1))
	if n, err := CountJobFailedAttempts(database, 102); err != nil || n != 1 {
		t.Fatalf("legacy failed completion: n=%d err=%v, want 1, nil", n, err)
	}

	// Job with completed status but NULL exit_code -> 1 (legacy runner
	// that died before writing status file leaves this shape; the R2
	// escalation fallback is specifically supposed to catch it).
	insertTestJob(t, database, 106, "echo g", "/tmp", StatusCompleted)
	if n, err := CountJobFailedAttempts(database, 106); err != nil || n != 1 {
		t.Fatalf("completed with NULL exit_code: n=%d err=%v, want 1, nil (legacy pre-fix shape)", n, err)
	}

	// Job with explicit failed status
	insertTestJob(t, database, 103, "echo d", "/tmp", StatusFailed)
	if n, err := CountJobFailedAttempts(database, 103); err != nil || n != 1 {
		t.Fatalf("failed status: n=%d err=%v, want 1, nil", n, err)
	}

	// Job with dead status
	insertTestJob(t, database, 104, "echo e", "/tmp", StatusDead)
	if n, err := CountJobFailedAttempts(database, 104); err != nil || n != 1 {
		t.Fatalf("dead status: n=%d err=%v, want 1, nil", n, err)
	}

	// Canceled is not a failure
	insertTestJob(t, database, 105, "echo f", "/tmp", StatusCanceled)
	if n, err := CountJobFailedAttempts(database, 105); err != nil || n != 0 {
		t.Fatalf("canceled job: n=%d err=%v, want 0, nil", n, err)
	}

	// Nil DB / zero ID should return 0,nil without panicking
	if n, err := CountJobFailedAttempts(nil, 102); err != nil || n != 0 {
		t.Fatalf("nil db: n=%d err=%v, want 0, nil", n, err)
	}
	if n, err := CountJobFailedAttempts(database, 0); err != nil || n != 0 {
		t.Fatalf("zero job id: n=%d err=%v, want 0, nil", n, err)
	}
}

func TestListRecentFailedUndiagnosedIncludesDerivedFailedCompletion(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now().Unix()

	cleanID, err := RecordQueued(database, "hostA", "/tmp", "echo clean", "clean")
	if err != nil {
		t.Fatalf("RecordQueued clean: %v", err)
	}
	if err := RecordCompletionByID(database, cleanID, 0, now); err != nil {
		t.Fatalf("RecordCompletionByID clean: %v", err)
	}
	failedID, err := RecordQueued(database, "hostA", "/tmp", "echo failed", "failed")
	if err != nil {
		t.Fatalf("RecordQueued failed: %v", err)
	}
	if err := RecordCompletionByID(database, failedID, 1, now); err != nil {
		t.Fatalf("RecordCompletionByID failed: %v", err)
	}
	failedJob, err := GetJobByID(database, failedID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if failedJob.Status != StatusFailed {
		t.Fatalf("failed completion status = %q, want %q", failedJob.Status, StatusFailed)
	}
	deadID, err := RecordQueued(database, "hostA", "/tmp", "echo dead", "dead")
	if err != nil {
		t.Fatalf("RecordQueued dead: %v", err)
	}
	if err := CloseAttempt(database, deadID, StatusDead, nil, now); err != nil {
		t.Fatalf("CloseAttempt dead: %v", err)
	}
	diagnosedID, err := RecordQueued(database, "hostA", "/tmp", "echo diagnosed", "diagnosed")
	if err != nil {
		t.Fatalf("RecordQueued diagnosed: %v", err)
	}
	if err := RecordCompletionByID(database, diagnosedID, 1, now); err != nil {
		t.Fatalf("RecordCompletionByID diagnosed: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET error_diagnosis = 'known' WHERE job_id = ?`, diagnosedID); err != nil {
		t.Fatalf("mark diagnosed: %v", err)
	}

	jobs, err := ListRecentFailedUndiagnosed(database, 10)
	if err != nil {
		t.Fatalf("ListRecentFailedUndiagnosed: %v", err)
	}
	got := make(map[int64]struct{}, len(jobs))
	for _, job := range jobs {
		got[job.ID] = struct{}{}
	}
	if _, ok := got[failedID]; !ok {
		t.Fatalf("jobs = %v, missing failed completion %d", got, failedID)
	}
	if _, ok := got[deadID]; !ok {
		t.Fatalf("jobs = %v, missing dead job %d", got, deadID)
	}
	for _, id := range []int64{cleanID, diagnosedID} {
		if _, ok := got[id]; ok {
			t.Fatalf("jobs = %v, should not include %d", got, id)
		}
	}
}

func TestInsertAndListLifecycleEvents(t *testing.T) {
	database := SetupTestDB(t)

	// Insert a few events
	events := []LifecycleEvent{
		{EventKind: EventRelaunchEligible, JobCount: 4},
		{EventKind: EventRelaunchSkippedNoOffers, GPUSpec: "4090 ≥20GB", JobCount: 4},
		{EventKind: EventRetryAutoTriggered, LaunchID: 42, GPUSpec: "4090 ≥20GB", Detail: "bootstrap_timeout"},
		{EventKind: EventRetryNoOffers, AttemptNumber: 1, MaxAttempts: 5, Detail: "backoff 30s"},
		{EventKind: EventReconcileBootstrapTimeout, LaunchID: 42, GPUSpec: "4090 ≥20GB"},
	}
	for i := range events {
		events[i].OccurredAt = time.Now().Unix() - int64(len(events)-i)
		if err := InsertLifecycleEvent(database, &events[i]); err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
	}

	// List all
	all, err := ListLifecycleEvents(database, LifecycleEventFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("expected 5 events, got %d", len(all))
	}

	// Filter by kind prefix
	relaunchEvents, err := ListLifecycleEvents(database, LifecycleEventFilter{KindPrefix: "relaunch."})
	if err != nil {
		t.Fatalf("list relaunch: %v", err)
	}
	if len(relaunchEvents) != 2 {
		t.Errorf("expected 2 relaunch events, got %d", len(relaunchEvents))
	}

	// Filter by exact kind
	noOfferEvents, err := ListLifecycleEvents(database, LifecycleEventFilter{Kind: EventRelaunchSkippedNoOffers})
	if err != nil {
		t.Fatalf("list no_offers: %v", err)
	}
	if len(noOfferEvents) != 1 {
		t.Errorf("expected 1 no_offers event, got %d", len(noOfferEvents))
	}

	// Filter by launch ID
	launchEvents, err := ListLifecycleEvents(database, LifecycleEventFilter{LaunchID: 42})
	if err != nil {
		t.Fatalf("list launch 42: %v", err)
	}
	if len(launchEvents) != 2 {
		t.Errorf("expected 2 events for launch 42, got %d", len(launchEvents))
	}

	// Verify fields roundtrip
	if noOfferEvents[0].GPUSpec != "4090 ≥20GB" {
		t.Errorf("gpu_spec = %q, want %q", noOfferEvents[0].GPUSpec, "4090 ≥20GB")
	}
	if noOfferEvents[0].JobCount != 4 {
		t.Errorf("job_count = %d, want 4", noOfferEvents[0].JobCount)
	}
}

func TestListLifecycleEventsAfterID(t *testing.T) {
	database := SetupTestDB(t)
	if id, err := LatestLifecycleEventID(database); err != nil || id != 0 {
		t.Fatalf("initial latest id = %d, err=%v; want 0, nil", id, err)
	}
	if err := InsertLifecycleEvent(database, &LifecycleEvent{EventKind: EventRelaunchEligible}); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	cursor, err := LatestLifecycleEventID(database)
	if err != nil {
		t.Fatalf("LatestLifecycleEventID: %v", err)
	}
	if cursor == 0 {
		t.Fatal("cursor should advance after first insert")
	}
	if err := InsertLifecycleEvent(database, &LifecycleEvent{EventKind: EventRelaunchLaunchSuccess, LaunchID: 7}); err != nil {
		t.Fatalf("insert second: %v", err)
	}
	if err := InsertLifecycleEvent(database, &LifecycleEvent{EventKind: EventRelaunchLaunchFailed, LaunchID: 8}); err != nil {
		t.Fatalf("insert third: %v", err)
	}

	events, err := ListLifecycleEventsAfterID(database, cursor, 10)
	if err != nil {
		t.Fatalf("ListLifecycleEventsAfterID: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2: %#v", len(events), events)
	}
	if events[0].EventKind != EventRelaunchLaunchSuccess || events[1].EventKind != EventRelaunchLaunchFailed {
		t.Fatalf("events not oldest-first after cursor: %#v", events)
	}
}

func TestInsertLifecycleEventNilDB(t *testing.T) {
	// Should not panic
	err := InsertLifecycleEvent(nil, &LifecycleEvent{EventKind: EventRelaunchEligible})
	if err != nil {
		t.Errorf("expected nil error for nil db, got %v", err)
	}
}

func TestInsertLifecycleEventDedup(t *testing.T) {
	database := SetupTestDB(t)
	const jobID = int64(901)
	now := time.Now().Unix()
	mk := func(detail string, occurredAt int64) *LifecycleEvent {
		return &LifecycleEvent{
			EventKind:  EventQueueDispatchFailed,
			JobID:      jobID,
			Detail:     detail,
			OccurredAt: occurredAt,
		}
	}

	// First insert always proceeds.
	if inserted, err := InsertLifecycleEventDedup(database, mk("rsync killed", now-90), 5*time.Minute); err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}
	// Same detail within the window → skipped.
	if inserted, err := InsertLifecycleEventDedup(database, mk("rsync killed", now-30), 5*time.Minute); err != nil || inserted {
		t.Fatalf("dup insert should skip: inserted=%v err=%v", inserted, err)
	}
	// Different detail within the window → inserts (different reason).
	if inserted, err := InsertLifecycleEventDedup(database, mk("ssh timeout", now-20), 5*time.Minute); err != nil || !inserted {
		t.Fatalf("different-detail insert: inserted=%v err=%v", inserted, err)
	}
	// Same detail but past the window → inserts (window scoped to recent).
	if inserted, err := InsertLifecycleEventDedup(database, mk("rsync killed", now-10), 5*time.Second); err != nil || !inserted {
		t.Fatalf("past-window insert: inserted=%v err=%v", inserted, err)
	}

	// Confirm rows count.
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM lifecycle_events WHERE job_id = ? AND event_kind = ?`, jobID, EventQueueDispatchFailed).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("rows = %d, want 3 (first, different-detail, past-window)", count)
	}
}

func TestCheckpointAutoPublishAttemptedWithinCooldown(t *testing.T) {
	database := SetupTestDB(t)
	const (
		jobID = int64(7)
		key   = "7:checkpoint:trace"
	)
	cooldown := 10 * time.Minute
	now := time.Unix(1_000_000, 0)

	if attempted, err := CheckpointAutoPublishAttemptedWithinCooldown(database, jobID, key, cooldown, now); err != nil || attempted {
		t.Fatalf("no attempt on record: attempted=%v err=%v, want false/nil", attempted, err)
	}

	// First process records an attempt; a second process's fresh gate — no
	// shared memory — reads it back from the DB within the cooldown window.
	if err := InsertLifecycleEvent(database, &LifecycleEvent{
		EventKind:  EventCheckpointAutoPublishAttempt,
		JobID:      jobID,
		Detail:     key,
		OccurredAt: now.Unix(),
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}
	if attempted, err := CheckpointAutoPublishAttemptedWithinCooldown(database, jobID, key, cooldown, now.Add(5*time.Minute)); err != nil || !attempted {
		t.Fatalf("within window: attempted=%v err=%v, want true/nil", attempted, err)
	}

	// The gate is keyed per (job, asset): other keys and other jobs are
	// independent of this attempt.
	if attempted, err := CheckpointAutoPublishAttemptedWithinCooldown(database, jobID, "7:checkpoint:other", cooldown, now.Add(5*time.Minute)); err != nil || attempted {
		t.Fatalf("different key: attempted=%v err=%v, want false/nil", attempted, err)
	}
	if attempted, err := CheckpointAutoPublishAttemptedWithinCooldown(database, 8, key, cooldown, now.Add(5*time.Minute)); err != nil || attempted {
		t.Fatalf("different job: attempted=%v err=%v, want false/nil", attempted, err)
	}

	// Once the cooldown window has elapsed, the gate reopens.
	if attempted, err := CheckpointAutoPublishAttemptedWithinCooldown(database, jobID, key, cooldown, now.Add(11*time.Minute)); err != nil || attempted {
		t.Fatalf("after window: attempted=%v err=%v, want false/nil", attempted, err)
	}

	// Nil DB / invalid args are a no-op, not an error.
	if attempted, err := CheckpointAutoPublishAttemptedWithinCooldown(nil, jobID, key, cooldown, now); err != nil || attempted {
		t.Fatalf("nil db: attempted=%v err=%v, want false/nil", attempted, err)
	}
	if attempted, err := CheckpointAutoPublishAttemptedWithinCooldown(database, 0, key, cooldown, now); err != nil || attempted {
		t.Fatalf("zero job id: attempted=%v err=%v, want false/nil", attempted, err)
	}
}

func TestCountActiveSourceSyncTimeoutHosts(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now()

	timeout := func(host string, at time.Time) {
		if err := InsertLifecycleEvent(database, &LifecycleEvent{
			EventKind: EventSourceSyncTimeout, Detail: host, OccurredAt: at.Unix(),
		}); err != nil {
			t.Fatalf("insert timeout: %v", err)
		}
	}
	ok := func(host string, at time.Time) {
		if err := InsertLifecycleEvent(database, &LifecycleEvent{
			EventKind: EventSourceSyncOK, Detail: host, OccurredAt: at.Unix(),
		}); err != nil {
			t.Fatalf("insert ok: %v", err)
		}
	}

	// host-a: active recent streak (2 timeouts, no OK since).
	timeout("host-a", now.Add(-3*time.Minute))
	timeout("host-a", now.Add(-1*time.Minute))
	// host-b: active recent streak.
	timeout("host-b", now.Add(-2*time.Minute))
	// host-c: recovered (OK after its timeout) -> not active.
	timeout("host-c", now.Add(-4*time.Minute))
	ok("host-c", now.Add(-30*time.Second))
	// host-d: only a stale timeout, outside the window -> not active.
	timeout("host-d", now.Add(-30*time.Minute))

	since := now.Add(-simultaneousWedgeWindowForTest)
	active, err := CountActiveSourceSyncTimeoutHosts(database, since)
	if err != nil {
		t.Fatalf("CountActiveSourceSyncTimeoutHosts: %v", err)
	}
	if active != 2 {
		t.Fatalf("active hosts = %d, want 2 (host-a, host-b)", active)
	}
}

// simultaneousWedgeWindowForTest mirrors ops.simultaneousWedgeWindow; the ops
// package owns the production constant, this keeps the db-level test self
// contained without importing ops (which would be an import cycle).
const simultaneousWedgeWindowForTest = 10 * time.Minute
