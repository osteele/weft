package db

import (
	"sync"
	"testing"
)

// TestConcurrentConflictingTerminalObservationsBarrier checks that when two
// independent DB connections race to apply conflicting terminal observations
// to a running external job, exactly one wins and the other is an idempotent
// no-op. This is the barrier-controlled two-connection version of the
// sequential stale-writer test: both writers start from the same running
// snapshot, but SQLite's immediate transaction locks serialize the commits.
func TestConcurrentConflictingTerminalObservationsBarrier(t *testing.T) {
	primary := SetupTestDBWithOpen(t)
	secondary, err := Open()
	if err != nil {
		t.Fatalf("open secondary connection: %v", err)
	}
	t.Cleanup(func() { secondary.Close() })

	binding, _, err := UpsertExternalJobFromObservation(primary, ExternalJobObservation{
		Executor:         ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		ExternalTaskID:   "task-a",
		RawStatus:        "RUNNING",
		NormalizedStatus: StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create running job: %v", err)
	}
	jobID := binding.JobID

	succeeded := ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "SUCCEEDED", NormalizedStatus: StatusCompleted,
	}
	failed := ExternalJobObservation{
		Executor: ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "FAILED", NormalizedStatus: StatusFailed,
	}

	const writers = 2
	start := make(chan struct{})
	type result struct {
		applied bool
		err     error
		status  string
		raw     string
	}
	results := make(chan result, writers)
	var ready sync.WaitGroup
	ready.Add(writers)

	go func() {
		ready.Done()
		<-start
		applied, err := ApplyExternalJobObservation(primary, jobID, succeeded)
		results <- result{applied: applied, err: err, status: StatusCompleted, raw: "SUCCEEDED"}
	}()
	go func() {
		ready.Done()
		<-start
		applied, err := ApplyExternalJobObservation(secondary, jobID, failed)
		results <- result{applied: applied, err: err, status: StatusFailed, raw: "FAILED"}
	}()

	ready.Wait()
	close(start)

	appliedCount := 0
	var winner result
	for range writers {
		r := <-results
		if r.err != nil {
			t.Fatalf("ApplyExternalJobObservation: %v", r.err)
		}
		if r.applied {
			appliedCount++
			winner = r
		}
	}
	if appliedCount != 1 {
		t.Fatalf("applied count = %d, want exactly 1", appliedCount)
	}

	job, err := GetJobByID(primary, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != winner.status {
		t.Fatalf("job status = %q, want winner %q", job.Status, winner.status)
	}

	refreshed, err := GetExternalJobBindingByJobID(primary, jobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if refreshed.RawStatus != winner.raw {
		t.Fatalf("binding raw status = %q, want winner %q", refreshed.RawStatus, winner.raw)
	}
}
