package orchestration

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// seedCancelledThenStarted creates a job whose cancel was requested at
// cancelAt and whose attempt then started at startAt. withInstance selects
// between a cloud attempt, which records a launch the agent polls, and an
// on-prem attempt, which records none.
func seedCancelledThenStarted(t *testing.T, database *sql.DB, cancelAt, startAt int64, attemptStatus string, withInstance bool) int64 {
	t.Helper()
	jobID, err := db.RecordQueued(database, "", "/tmp/p", "echo hi", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE jobs SET requested_status = ?, cancel_requested_at = ? WHERE id = ?`,
		db.StatusCanceled, cancelAt, jobID); err != nil {
		t.Fatalf("set cancel state: %v", err)
	}
	var launch *int64
	if withInstance {
		launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "H200"})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		launch = &launchID
	}
	attemptID, err := db.CreateAttempt(database, jobID, "", launch, attemptStatus)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET start_time = ?, status = ? WHERE id = ?`,
		startAt, attemptStatus, attemptID); err != nil {
		t.Fatalf("set attempt start: %v", err)
	}
	return jobID
}

// TestStopJobsStartedAfterCancelStopsOnlyPostCancelStarts guards wb72. The
// sweep terminates work, so a false positive kills a job someone is waiting
// on. It must act on a job whose start followed its cancel, and leave alone
// one that was already running when the cancel landed.
func TestStopJobsStartedAfterCancelStopsOnlyPostCancelStarts(t *testing.T) {
	database := db.SetupTestDB(t)
	cancelAt := time.Now().Add(-10 * time.Minute).Unix()

	afterCancel := seedCancelledThenStarted(t, database, cancelAt, cancelAt+60, db.StatusRunning, true)
	beforeCancel := seedCancelledThenStarted(t, database, cancelAt, cancelAt-60, db.StatusRunning, true)

	var signalled []int64
	stubSignal(t, func(jobID, launchID int64) error {
		signalled = append(signalled, jobID)
		return nil
	})

	stopped, err := StopJobsStartedAfterCancel(database)
	if err != nil {
		t.Fatalf("StopJobsStartedAfterCancel: %v", err)
	}
	if stopped != 1 {
		t.Fatalf("stopped %d jobs, want exactly the one started after its cancel", stopped)
	}
	if len(signalled) != 1 || signalled[0] != afterCancel {
		t.Fatalf("signalled %v, want exactly the post-cancel job %d", signalled, afterCancel)
	}

	// The swept job must no longer be selectable, so a later pass does not
	// re-stop it and the sweep is idempotent. The stub writes no state, so this
	// exercises the sweep's own escalation.
	if selectable(t, database, afterCancel) {
		t.Fatalf("job %d still selectable after being swept; sweep is not idempotent", afterCancel)
	}

	// The job that was already running keeps its cancel intent and is
	// untouched by the sweep.
	if _, ok, err := db.CancelRequestedAt(database, beforeCancel); err != nil || !ok {
		t.Fatalf("pre-cancel-start job lost its cancel stamp: recorded=%v err=%v", ok, err)
	}
}

// A clean database must produce no work and no error, since the sweep runs on
// every autopilot pass.
func TestStopJobsStartedAfterCancelNoopWhenNothingQualifies(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.RecordQueued(database, "", "/tmp/p", "echo hi", "test"); err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	stopped, err := StopJobsStartedAfterCancel(database)
	if err != nil {
		t.Fatalf("StopJobsStartedAfterCancel: %v", err)
	}
	if stopped != 0 {
		t.Fatalf("stopped %d jobs on a database with nothing to stop", stopped)
	}
}

// A kill marker that fails to write must leave the start selectable, so the
// next pass retries it. Escalating the intent first would drop the job out of
// selection on a timed-out write and abandon the cancel the user asked for.
func TestStopJobsStartedAfterCancelRetriesWhenSignalFails(t *testing.T) {
	database := db.SetupTestDB(t)
	cancelAt := time.Now().Add(-10 * time.Minute).Unix()
	jobID := seedCancelledThenStarted(t, database, cancelAt, cancelAt+60, db.StatusRunning, true)

	stubSignal(t, func(int64, int64) error { return errors.New("R2 unreachable") })

	stopped, err := StopJobsStartedAfterCancel(database)
	if err == nil {
		t.Fatal("StopJobsStartedAfterCancel reported success for a signal that failed")
	}
	if stopped != 0 {
		t.Fatalf("counted %d jobs as stopped when the signal failed", stopped)
	}
	if !selectable(t, database, jobID) {
		t.Fatalf("job %d dropped out of selection after a failed signal; the cancel would never be retried", jobID)
	}
}

// A post-cancel start with no instance has no agent polling a marker. It must
// leave selection so the sweep does not revisit it every pass, and must land as
// a closed attempt rather than a job reading killed over an open one.
func TestStopJobsStartedAfterCancelLandsStartWithNoInstance(t *testing.T) {
	database := db.SetupTestDB(t)
	cancelAt := time.Now().Add(-10 * time.Minute).Unix()
	jobID := seedCancelledThenStarted(t, database, cancelAt, cancelAt+60, db.StatusRunning, false)

	stubSignal(t, func(int64, int64) error {
		t.Fatal("signalled a start that records no instance")
		return nil
	})

	stopped, err := StopJobsStartedAfterCancel(database)
	if err == nil {
		t.Fatal("no error reported for a start that could not be signalled")
	}
	if stopped != 0 {
		t.Fatalf("counted %d jobs as stopped without signalling any", stopped)
	}
	if selectable(t, database, jobID) {
		t.Fatalf("job %d still selectable; the sweep would revisit it every pass", jobID)
	}

	var status string
	var endTime *int64
	if err := database.QueryRow(
		`SELECT status, end_time FROM job_attempts WHERE job_id = ? ORDER BY id DESC LIMIT 1`,
		jobID).Scan(&status, &endTime); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if status != db.StatusKilled {
		t.Fatalf("attempt status = %q, want %q", status, db.StatusKilled)
	}
	if endTime == nil || *endTime == 0 {
		t.Fatal("attempt left open with no end_time while the job reads killed")
	}
}

// stubSignal replaces the R2 kill-marker seam for one test. The stub writes no
// database state, so assertions about selection exercise the sweep's own
// escalation rather than the stub's.
func stubSignal(t *testing.T, fn func(jobID, launchID int64) error) {
	t.Helper()
	prev := signalCloudJobKillFunc
	t.Cleanup(func() { signalCloudJobKillFunc = prev })
	signalCloudJobKillFunc = func(_ context.Context, jobID, launchID int64, _ string) (string, error) {
		if err := fn(jobID, launchID); err != nil {
			return "", err
		}
		return "signalled", nil
	}
}

// selectable reports whether the sweep would pick the job up again.
func selectable(t *testing.T, database *sql.DB, jobID int64) bool {
	t.Helper()
	starts, err := db.JobsStartedAfterCancel(database)
	if err != nil {
		t.Fatalf("JobsStartedAfterCancel: %v", err)
	}
	for _, s := range starts {
		if s.JobID == jobID {
			return true
		}
	}
	return false
}
