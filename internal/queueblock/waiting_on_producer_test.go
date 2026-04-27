package queueblock

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func setProducerNeeds(t *testing.T, database *sql.DB, jobID int64, needs string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE jobs SET needs = ? WHERE id = ?`, needs, jobID); err != nil {
		t.Fatalf("set needs: %v", err)
	}
}

// ensureRentalLaunch inserts a launches row referenced by attempts so the
// job_status view's cloud-jobs branch fires (which produces stable status
// derivations for terminal rental jobs).
func ensureRentalLaunch(t *testing.T, database *sql.DB, launchID int64, status string) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO launches (id, status, provider, created_at) VALUES (?, ?, 'vastai', ?)`,
		launchID, status, time.Now().Unix(),
	); err != nil {
		t.Fatalf("insert launch: %v", err)
	}
}

// upsertAttempt creates or replaces the latest attempt row for a job so the
// job_status view yields the desired effective status. RecordQueuedWithGPUAndID
// with host="" does not create an attempt, so we always start by inserting one.
// When launchID > 0, the attempt references that launches row (cloud-job branch
// in the view); otherwise the on-prem branch fires.
func upsertAttempt(t *testing.T, database *sql.DB, jobID int64, status, host string, launchID int64, terminal bool) {
	t.Helper()
	if _, err := database.Exec(`DELETE FROM job_attempts WHERE job_id = ?`, jobID); err != nil {
		t.Fatalf("clear attempts: %v", err)
	}
	now := time.Now().Unix()
	var (
		startTime *int64
		endTime   *int64
		exitCode  sql.NullInt64
		launchPtr *int64
	)
	if launchID > 0 {
		launchPtr = &launchID
	}
	if status == db.StatusRunning || db.IsTerminalStatus(status) {
		s := now - 10
		startTime = &s
	}
	if terminal {
		e := now
		endTime = &e
		switch status {
		case db.StatusCompleted:
			exitCode = sql.NullInt64{Int64: 0, Valid: true}
		case db.StatusFailed:
			exitCode = sql.NullInt64{Int64: 1, Valid: true}
		}
	}
	if _, err := database.Exec(
		`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, queued_at, start_time, end_time, exit_code)
		 VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, host, launchPtr, status, now-20, startTime, endTime, exitCode,
	); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
}

func makeConsumer(needs ...string) *db.Job {
	return &db.Job{ID: 9999, Needs: needs}
}

// rentalHost returns the legacy synthetic rental host name. We use this in
// tests instead of inserting launches rows because TargetKind() recognises
// the "vastai:N" pattern as a rental target without requiring a foreign-key
// match in the launches table.
func rentalHost() string { return db.LaunchHost(1) }

func TestWaitingOnProducer_RunningRentalProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 100, "", "/tmp/p", "python p.py", "producer", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID: %v", err)
	}
	ensureRentalLaunch(t, database, 1, "running")
	upsertAttempt(t, database, 100, db.StatusRunning, rentalHost(), 1, false)

	reason, blocked := WaitingOnProducerReason(database, makeConsumer("output/m.pt:100"))
	if !blocked {
		t.Fatalf("expected blocked, got reason=%q", reason)
	}
	if !strings.Contains(reason, "(running)") {
		t.Errorf("reason missing (running): %q", reason)
	}
	if !strings.Contains(reason, `"output/m.pt"`) {
		t.Errorf("reason missing quoted path: %q", reason)
	}
	if !strings.Contains(reason, "wj100") {
		t.Errorf("reason missing wj100: %q", reason)
	}
}

func TestWaitingOnProducer_QueuedUnplacedProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 101, "", "/tmp/p", "python p.py", "producer", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID: %v", err)
	}

	reason, blocked := WaitingOnProducerReason(database, makeConsumer("output/m.pt:101"))
	if !blocked {
		t.Fatalf("expected blocked, got reason=%q", reason)
	}
	if !strings.Contains(reason, "(queued)") {
		t.Errorf("reason missing (queued): %q", reason)
	}
}

func TestWaitingOnProducer_CompletedRentalProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 102, "", "/tmp/p", "python p.py", "producer", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID: %v", err)
	}
	ensureRentalLaunch(t, database, 2, "completed")
	upsertAttempt(t, database, 102, db.StatusCompleted, rentalHost(), 2, true)

	reason, blocked := WaitingOnProducerReason(database, makeConsumer("output/m.pt:102"))
	if blocked {
		t.Fatalf("expected not blocked, got reason=%q", reason)
	}
}

func TestWaitingOnProducer_CompletedOnPremProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 103, "cool30", "/tmp/p", "python p.py", "producer", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID: %v", err)
	}
	upsertAttempt(t, database, 103, db.StatusCompleted, "cool30", 0, true)

	reason, blocked := WaitingOnProducerReason(database, makeConsumer("output/m.pt:103"))
	if !blocked {
		t.Fatalf("expected blocked, got reason=%q", reason)
	}
	if !strings.Contains(reason, "on-prem, not in R2") {
		t.Errorf("reason missing on-prem note: %q", reason)
	}
}

func TestWaitingOnProducer_FailedRentalProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 104, "", "/tmp/p", "python p.py", "producer", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID: %v", err)
	}
	ensureRentalLaunch(t, database, 4, "failed")
	upsertAttempt(t, database, 104, db.StatusFailed, rentalHost(), 4, true)

	reason, blocked := WaitingOnProducerReason(database, makeConsumer("output/m.pt:104"))
	if !blocked {
		t.Fatalf("expected blocked, got reason=%q", reason)
	}
	if !strings.Contains(reason, "failed") {
		t.Errorf("reason missing failed status: %q", reason)
	}
}

func TestWaitingOnProducer_MissingProducer(t *testing.T) {
	database := db.SetupTestDB(t)

	reason, blocked := WaitingOnProducerReason(database, makeConsumer("output/m.pt:9001"))
	if !blocked {
		t.Fatalf("expected blocked, got reason=%q", reason)
	}
	if !strings.Contains(reason, "producer not found") {
		t.Errorf("reason missing missing-producer note: %q", reason)
	}
}

func TestWaitingOnProducer_FirstSatisfiedSecondBlocked(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 105, "", "/tmp/p", "python p.py", "producer-a", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID 105: %v", err)
	}
	ensureRentalLaunch(t, database, 5, "completed")
	upsertAttempt(t, database, 105, db.StatusCompleted, rentalHost(), 5, true)
	if err := db.RecordQueuedWithGPUAndID(database, 106, "", "/tmp/p", "python p.py", "producer-b", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID 106: %v", err)
	}
	ensureRentalLaunch(t, database, 6, "running")
	upsertAttempt(t, database, 106, db.StatusRunning, rentalHost(), 6, false)

	reason, blocked := WaitingOnProducerReason(database, makeConsumer("a.pt:105", "b.pt:106"))
	if !blocked {
		t.Fatalf("expected blocked, got reason=%q", reason)
	}
	if !strings.Contains(reason, "wj106") || !strings.Contains(reason, `"b.pt"`) {
		t.Errorf("expected reason about wj106/b.pt, got %q", reason)
	}
}

func TestWaitingOnProducer_NoNeeds(t *testing.T) {
	database := db.SetupTestDB(t)

	reason, blocked := WaitingOnProducerReason(database, &db.Job{ID: 1})
	if blocked {
		t.Fatalf("expected not blocked, got %q", reason)
	}
}

func TestWaitingOnProducer_NeedsRoundTripFromDB(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 200, "", "/tmp/c", "python c.py", "consumer", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID 200: %v", err)
	}
	if err := db.RecordQueuedWithGPUAndID(database, 201, "", "/tmp/p", "python p.py", "producer", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID 201: %v", err)
	}
	ensureRentalLaunch(t, database, 201, "running")
	upsertAttempt(t, database, 201, db.StatusRunning, rentalHost(), 201, false)
	setProducerNeeds(t, database, 200, `["output/m.pt:201"]`)

	consumer, err := db.GetJobByID(database, 200)
	if err != nil || consumer == nil {
		t.Fatalf("GetJobByID(200): %v %v", consumer, err)
	}
	reason, blocked := WaitingOnProducerReason(database, consumer)
	if !blocked || !strings.Contains(reason, "wj201") {
		t.Fatalf("expected blocked on wj201, got blocked=%v reason=%q", blocked, reason)
	}
}
