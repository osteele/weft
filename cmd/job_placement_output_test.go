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
