package skypilot

import (
	"context"
	"sync"
	"testing"

	"github.com/osteele/weft/internal/db"
)

type delayedSnapshotRunner struct {
	mu           sync.Mutex
	calls        int
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (r *delayedSnapshotRunner) Run(context.Context, ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 1 {
		close(r.firstStarted)
		<-r.releaseFirst
		return []byte(`[{"job_id":42,"status":"RUNNING"}]`), nil
	}
	return []byte(`[{"job_id":42,"status":"SUCCEEDED"}]`), nil
}

func TestConcurrentSyncDoesNotLetDelayedRunningReopenTerminalJob(t *testing.T) {
	database := db.SetupTestDBWithOpen(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor:         db.ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "RUNNING",
		NormalizedStatus: db.StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	runner := &delayedSnapshotRunner{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	client := Client{Runner: runner}
	type result struct {
		updated int
		err     error
	}
	firstDone := make(chan result, 1)
	go func() {
		updated, _, err := client.SyncBindingsAmbient(context.Background(), database, "")
		firstDone <- result{updated: updated, err: err}
	}()
	<-runner.firstStarted

	updated, _, err := client.SyncBindingsAmbient(context.Background(), database, "")
	if err != nil {
		t.Fatalf("terminal sync: %v", err)
	}
	if updated != 1 {
		t.Fatalf("terminal sync updated = %d, want 1", updated)
	}
	terminalBinding, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get terminal binding: %v", err)
	}
	if terminalBinding.LastObservedAt == nil {
		t.Fatal("terminal observation has no timestamp")
	}
	terminalObservedAt := *terminalBinding.LastObservedAt
	close(runner.releaseFirst)
	first := <-firstDone
	if first.err != nil {
		t.Fatalf("delayed running sync: %v", first.err)
	}
	if first.updated != 0 {
		t.Fatalf("delayed running sync updated = %d, want 0", first.updated)
	}

	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	if job.Status != db.StatusCompleted || job.EndTime == nil {
		t.Fatalf("status, end_time = %q, %v; want completed with end time", job.Status, job.EndTime)
	}
	finalBinding, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get final binding: %v", err)
	}
	if finalBinding.RawStatus != "SUCCEEDED" || finalBinding.LastObservedAt == nil || *finalBinding.LastObservedAt != terminalObservedAt {
		t.Fatalf("final binding = raw %q observed %v; want SUCCEEDED at %d", finalBinding.RawStatus, finalBinding.LastObservedAt, terminalObservedAt)
	}
}

func TestFindJobForTaskQualifiedBindingRequiresExactJobID(t *testing.T) {
	binding := &db.ExternalJobBinding{ExternalJobID: "42", ExternalTaskID: "task-a"}
	job, ok, _ := findJobForBinding([]Job{{
		ID: "99", Name: "42", TaskID: "task-a", Status: "SUCCEEDED",
	}}, binding)
	if ok {
		t.Fatalf("findJobForBinding = %+v, true; want no match for wrong job ID", job)
	}
}

func TestSyncRefinesUnqualifiedBindingWithoutCreatingDuplicate(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, attemptID, err := db.CreateExternalPendingJob(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, RawStatus: "submitted",
		NormalizedStatus: db.StatusQueued, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create pending job: %v", err)
	}
	if err := db.AttachExternalBinding(database, jobID, attemptID, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "SUBMITTED", NormalizedStatus: db.StatusQueued,
	}); err != nil {
		t.Fatalf("attach labeled-ID fallback: %v", err)
	}
	client := Client{Runner: tuiRunner{out: []byte(`[{"job_id":42,"task_id":"task-a","status":"RUNNING"}]`)}}
	updated, missing, err := client.SyncBindingsAmbient(context.Background(), database, "")
	if err != nil {
		t.Fatalf("sync refined identity: %v", err)
	}
	if updated != 1 || missing != 0 {
		t.Fatalf("updated, missing = %d, %d; want 1, 0", updated, missing)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM external_job_bindings WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	binding, err := db.GetExternalJobBindingByJobID(database, jobID)
	if err != nil {
		t.Fatalf("get refined binding: %v", err)
	}
	if count != 1 || binding == nil || binding.ExternalTaskID != "task-a" {
		t.Fatalf("binding count/value = %d, %+v; want one task-a binding", count, binding)
	}
}

func TestSyncKeepsWindingDownJobRunning(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create running binding: %v", err)
	}
	client := Client{Runner: tuiRunner{out: []byte(`[{"job_id":42,"status":"WINDING_DOWN"}]`)}}
	updated, missing, err := client.SyncBindingsAmbient(context.Background(), database, "")
	if err != nil || updated != 1 || missing != 0 {
		t.Fatalf("sync winding down = updated %d, missing %d, err %v", updated, missing, err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if job.Status != db.StatusRunning || job.EndTime != nil || refreshed.RawStatus != "WINDING_DOWN" {
		t.Fatalf("winding-down job/binding = status %q, end %v, raw %q", job.Status, job.EndTime, refreshed.RawStatus)
	}
}

type tuiRunner struct{ out []byte }

func (r tuiRunner) Run(context.Context, ...string) ([]byte, error) { return r.out, nil }
