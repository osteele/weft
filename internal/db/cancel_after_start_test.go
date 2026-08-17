package db

import (
	"testing"
	"time"
)

// TestJobsStartedAfterCancelRequiresPositiveEvidence guards wb72. The caller
// terminates on this result, so every exclusion here is load-bearing: a false
// positive kills work the user is waiting on.
func TestJobsStartedAfterCancelRequiresPositiveEvidence(t *testing.T) {
	database := SetupTestDB(t)
	cancelAt := time.Now().Add(-10 * time.Minute).Unix()

	// Seeds a job with the given cancel stamp and one attempt.
	seed := func(t *testing.T, name string, requested string, cancelStamp *int64, startTime *int64, attemptStatus string) int64 {
		t.Helper()
		jobID, err := RecordQueued(database, "", "/tmp/p", "echo "+name, "test")
		if err != nil {
			t.Fatalf("%s: RecordQueued: %v", name, err)
		}
		if _, err := database.Exec(`UPDATE jobs SET requested_status = ?, cancel_requested_at = ? WHERE id = ?`,
			requested, cancelStamp, jobID); err != nil {
			t.Fatalf("%s: set cancel state: %v", name, err)
		}
		attemptID, err := CreateAttempt(database, jobID, "host-alpha", nil, attemptStatus)
		if err != nil {
			t.Fatalf("%s: CreateAttempt: %v", name, err)
		}
		if _, err := database.Exec(`UPDATE job_attempts SET start_time = ?, status = ? WHERE id = ?`,
			startTime, attemptStatus, attemptID); err != nil {
			t.Fatalf("%s: set start: %v", name, err)
		}
		return jobID
	}

	after := cancelAt + 60
	before := cancelAt - 60

	qualifies := seed(t, "started-after-cancel", StatusCanceled, &cancelAt, &after, StatusRunning)

	// Each of these must be excluded, for a different reason.
	seed(t, "started-before-cancel", StatusCanceled, &cancelAt, &before, StatusRunning)
	seed(t, "no-cancel-time-recorded", StatusCanceled, nil, &after, StatusRunning)
	seed(t, "no-cancel-intent", StatusQueued, nil, &after, StatusRunning)
	seed(t, "start-not-observed", StatusCanceled, &cancelAt, nil, StatusRunning)
	seed(t, "already-terminal", StatusCanceled, &cancelAt, &after, StatusCompleted)
	sameInstant := cancelAt
	seed(t, "started-same-second", StatusCanceled, &cancelAt, &sameInstant, StatusRunning)

	got, err := JobsStartedAfterCancel(database)
	if err != nil {
		t.Fatalf("JobsStartedAfterCancel: %v", err)
	}
	if len(got) != 1 {
		ids := make([]int64, len(got))
		for i, g := range got {
			ids[i] = g.JobID
		}
		t.Fatalf("matched %d jobs %v, want exactly the one started after its cancel", len(got), ids)
	}
	if got[0].JobID != qualifies {
		t.Fatalf("matched job %d, want %d", got[0].JobID, qualifies)
	}
	if !got[0].StartedAt.After(got[0].CancelRequestedAt) {
		t.Fatalf("reported start %v is not after cancel %v", got[0].StartedAt, got[0].CancelRequestedAt)
	}
}

// A cancel stamps its time; anything else clears it, so a stale cancel time
// cannot outlive the intent it records and re-trigger a kill after a requeue.
func TestSetRequestedStatusStampsAndClearsCancelTime(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "", "/tmp/p", "echo hi", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	if _, ok, err := CancelRequestedAt(database, jobID); err != nil || ok {
		t.Fatalf("fresh job: got recorded=%v err=%v, want no cancel time", ok, err)
	}

	if err := SetRequestedStatus(database, jobID, StatusCanceled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	at, ok, err := CancelRequestedAt(database, jobID)
	if err != nil || !ok {
		t.Fatalf("after cancel: recorded=%v err=%v, want a cancel time", ok, err)
	}
	if time.Since(at) > time.Minute {
		t.Fatalf("cancel time %v is not recent", at)
	}

	if err := SetRequestedStatus(database, jobID, StatusQueued); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if _, ok, err := CancelRequestedAt(database, jobID); err != nil || ok {
		t.Fatalf("after requeue: recorded=%v err=%v, want the cancel time cleared", ok, err)
	}
}
