package cmd

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// createInventoryRunningAheadPair queues two jobs on one inventory host and
// moves the first to running, so the second reads as head-of-line queued.
func createInventoryRunningAheadPair(t *testing.T, database *sql.DB) (aheadID, queuedID int64) {
	t.Helper()

	host := "host-alpha"
	aheadID, err := db.RecordQueuedWithGPU(database, host, "/tmp", "echo ahead", "ahead job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(ahead): %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusRunning, time.Now().Unix(), aheadID); err != nil {
		t.Fatalf("set running attempt: %v", err)
	}
	queuedID, err = db.RecordQueuedWithGPU(database, host, "/tmp", "echo queued", "queued job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(queued): %v", err)
	}
	return aheadID, queuedID
}

func TestQueuedActionBlockerOutranksHeadOfLine(t *testing.T) {
	database := db.SetupTestDB(t)
	aheadID, queuedID := createInventoryRunningAheadPair(t, database)
	job, err := db.GetJobByID(database, queuedID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	job.QueueBlockedReason = "remote publication failed: ssh timeout"

	action := queuedAction(database, job)
	if !strings.Contains(action, "remote publication failed") {
		t.Fatalf("action does not name the recorded blocker, got: %s", action)
	}
	blockerAt := strings.Index(action, "remote publication failed")
	behindAt := strings.Index(action, "queued behind")
	if behindAt < 0 {
		t.Fatalf("action dropped the true head-of-line context, got: %s", action)
	}
	if behindAt < blockerAt {
		t.Fatalf("head-of-line attribution precedes the operative blocker, got: %s", action)
	}
	if !strings.Contains(action, "(also queued behind "+ids.FormatJobID(aheadID)+")") {
		t.Fatalf("head-of-line context is not subordinated as context, got: %s", action)
	}
}

func TestQueuedActionBlockerWithoutAheadJob(t *testing.T) {
	database := db.SetupTestDB(t)
	id, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp", "echo hi", "queued job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	job.QueueBlockedReason = "remote publication failed: ssh timeout"

	action := queuedAction(database, job)
	if !strings.Contains(action, "remote publication failed") {
		t.Fatalf("action does not name the recorded blocker, got: %s", action)
	}
	if strings.Contains(action, "queued behind") {
		t.Fatalf("action invents head-of-line attribution with no job ahead, got: %s", action)
	}
}

func TestQueuedActionHeadOfLinePhrasingUnchangedWithoutBlocker(t *testing.T) {
	// Guard: a job that is genuinely only waiting its turn keeps the
	// existing phrasing.
	database := db.SetupTestDB(t)
	aheadID, queuedID := createInventoryRunningAheadPair(t, database)
	job, err := db.GetJobByID(database, queuedID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	want := "wait; queued behind " + ids.FormatJobID(aheadID) +
		", monitor with weft status " + ids.FormatJobID(queuedID) + " --wait"
	if got := queuedAction(database, job); got != want {
		t.Fatalf("action = %q, want %q", got, want)
	}
}

func TestQueuedActionPlainWaitUnchangedWithoutBlockerOrAheadJob(t *testing.T) {
	database := db.SetupTestDB(t)
	id, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp", "echo hi", "queued job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	want := "wait; no manual retry or kill indicated, monitor with weft status " +
		ids.FormatJobID(id) + " --wait"
	if got := queuedAction(database, job); got != want {
		t.Fatalf("action = %q, want %q", got, want)
	}
}

func TestQueueReasonSummaryBlockerOutranksHeadOfLine(t *testing.T) {
	database := db.SetupTestDB(t)
	aheadID, queuedID := createInventoryRunningAheadPair(t, database)
	job, err := db.GetJobByID(database, queuedID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	job.QueueBlockedReason = "remote publication failed: ssh timeout"

	reason := queueReasonSummary(database, job)
	if !strings.HasPrefix(reason, "blocked: remote publication failed") {
		t.Fatalf("queue reason does not lead with the operative blocker, got: %s", reason)
	}
	if !strings.Contains(reason, "(also waiting behind "+ids.FormatJobID(aheadID)+")") {
		t.Fatalf("queue reason dropped head-of-line context, got: %s", reason)
	}
}

func TestQueueReasonSummaryHeadOfLinePhrasingUnchangedWithoutBlocker(t *testing.T) {
	database := db.SetupTestDB(t)
	aheadID, queuedID := createInventoryRunningAheadPair(t, database)
	job, err := db.GetJobByID(database, queuedID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	want := "waiting behind " + ids.FormatJobID(aheadID)
	if got := queueReasonSummary(database, job); got != want {
		t.Fatalf("queue reason = %q, want %q", got, want)
	}
}

// wb181: status named wj8967 as the job ahead, and kept a growing wait, for a
// job that had already failed on studio — while wj8967 had itself completed.
// Both rows were stale because the host had stopped publishing runner state.
// Queue position is only as good as that publication, so while it is stale the
// answer is "unknown", not a job id.
func TestQueueReasonSummaryRefusesPositionWhileHostPublicationIsStale(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name        string
		observedAgo time.Duration
		wantUnknown bool
	}{
		{name: "fresh publication keeps the position", observedAgo: time.Minute},
		{name: "stale publication withholds it", observedAgo: hostAgentRuntimeFreshness + time.Minute, wantUnknown: true},
		{name: "future-dated observation is not fresh", observedAgo: -time.Hour, wantUnknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := db.SetupTestDB(t)
			aheadID, queuedID := createInventoryRunningAheadPair(t, database)
			if err := db.RecordHostAgentRuntime(database, "host-alpha", "agent-v1", 1, now.Add(-tc.observedAgo)); err != nil {
				t.Fatalf("RecordHostAgentRuntime: %v", err)
			}
			job, err := db.GetJobByID(database, queuedID)
			if err != nil {
				t.Fatalf("GetJobByID: %v", err)
			}
			// The job is on the host's queue; an undispatched one is covered
			// by TestQueueReasonSummaryLeavesUndispatchedJobsToDispatchReasons.
			job.LastSyncedStatus = db.StatusQueued

			reason := queueReasonSummary(database, job)
			action := queuedAction(database, job)
			ahead := ids.FormatJobID(aheadID)
			if !tc.wantUnknown {
				if reason != "waiting behind "+ahead || !strings.Contains(action, "queued behind "+ahead) {
					t.Fatalf("fresh publication lost the position: reason=%q action=%q", reason, action)
				}
				return
			}
			if strings.Contains(reason, ahead) || strings.Contains(action, ahead) {
				t.Fatalf("named a job ahead from an unobserved queue: reason=%q action=%q", reason, action)
			}
			for _, text := range []string{reason, action} {
				if !strings.Contains(text, "host-alpha") || !strings.Contains(text, "unknown") {
					t.Fatalf("stale publication was not reported as unknown: %q", text)
				}
			}
		})
	}
}

// A stopped daemon halts dispatch and ages the publication together, so for a
// job that has not reached the host's queue the unobserved-queue observation
// would displace the one cause an operator can act on.
func TestQueueReasonSummaryLeavesUndispatchedJobsToDispatchReasons(t *testing.T) {
	database := db.SetupTestDB(t)
	queuedID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp", "echo queued", "queued job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.RecordHostAgentRuntime(database, "host-alpha", "agent-v1", 1,
		time.Now().Add(-(hostAgentRuntimeFreshness + time.Hour))); err != nil {
		t.Fatalf("RecordHostAgentRuntime: %v", err)
	}
	job, err := db.GetJobByID(database, queuedID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	job.LastSyncedStatus = ""

	reason := queueReasonSummary(database, job)
	if strings.Contains(reason, "queue position") {
		t.Fatalf("undispatched job was told its queue position is unknown: %q", reason)
	}
	if !strings.Contains(reason, "daemon") {
		t.Fatalf("queue reason = %q, want the dispatch-side daemon reason", reason)
	}
}

// A host that stops publishing often stops because the thing the blocker names
// has failed, so the unobserved queue must qualify the blocker rather than
// replace it: the blocker is recorded on this job's own row and an unread
// queue does not make it less true.
func TestQueueReasonSummaryKeepsRecordedBlockerWhilePublicationIsStale(t *testing.T) {
	database := db.SetupTestDB(t)
	aheadID, queuedID := createInventoryRunningAheadPair(t, database)
	if err := db.RecordHostAgentRuntime(database, "host-alpha", "agent-v1", 1,
		time.Now().Add(-(hostAgentRuntimeFreshness + time.Minute))); err != nil {
		t.Fatalf("RecordHostAgentRuntime: %v", err)
	}
	job, err := db.GetJobByID(database, queuedID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	job.LastSyncedStatus = db.StatusQueued
	job.QueueBlockedReason = "remote publication failed: ssh timeout"

	reason := queueReasonSummary(database, job)
	action := queuedAction(database, job)
	for _, text := range []string{reason, action} {
		if !strings.Contains(text, "remote publication failed") {
			t.Fatalf("stale publication suppressed the recorded blocker: %q", text)
		}
		if !strings.Contains(text, "unknown") {
			t.Fatalf("blocker dropped the unobserved-queue qualifier: %q", text)
		}
		if strings.Contains(text, ids.FormatJobID(aheadID)) {
			t.Fatalf("named a job ahead from an unobserved queue: %q", text)
		}
		if blockerAt, unknownAt := strings.Index(text, "remote publication failed"), strings.Index(text, "unknown"); unknownAt < blockerAt {
			t.Fatalf("unobserved-queue qualifier preceded the operative blocker: %q", text)
		}
	}
}

func TestQueueReasonSummaryUnplacedKeepsDiagnosePointer(t *testing.T) {
	// Guard: the blocker-first ordering is scoped to placed targets. An
	// unplaced job keeps the phrasing that points at weft diagnose.
	database := db.SetupTestDB(t)
	id, err := db.RecordQueuedWithGPU(database, "", "/tmp", "python train.py", "queued job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, id)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	job.QueueBlockedReason = "no rental headroom; disk insufficient"

	want := "blocked: no rental headroom; disk insufficient - see weft diagnose job " + ids.FormatJobID(id)
	if got := queueReasonSummary(database, job); got != want {
		t.Fatalf("queue reason = %q, want %q", got, want)
	}
}

func TestRecordedBlockerEmptyForHealthyQueuedJob(t *testing.T) {
	// A job with nothing recorded must read as unblocked: a false
	// blocker here would hijack every healthy queue line.
	database := db.SetupTestDB(t)
	id, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp", "echo hi", "queued job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	if blocker := recordedBlocker(database, job); blocker != "" {
		t.Fatalf("recordedBlocker = %q for a healthy queued job, want empty", blocker)
	}
}
