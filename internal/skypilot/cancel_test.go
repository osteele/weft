package skypilot

import (
	"context"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestCancelBindingRefusesTaskScopedCancelWhenSiblingTasksAreVisible(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	runner := &scriptedRunner{results: []scriptedRunResult{{out: []byte(`[
		{"job_id":42,"task_id":"task-a","status":"RUNNING"},
		{"job_id":42,"task_id":"task-b","status":"RUNNING"}
	]`)}}}

	requested, err := (Client{Runner: runner}).CancelBinding(context.Background(), database, binding)
	if err == nil || !strings.Contains(err.Error(), "2 visible tasks") || requested {
		t.Fatalf("CancelBinding = %t, %v; want sibling-scope refusal", requested, err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want queue observation only", runner.calls)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if refreshed.CancelRequestedAt != nil {
		t.Fatalf("cancel_requested_at = %v, want nil after refused broad cancel", refreshed.CancelRequestedAt)
	}
}

func TestCancelBindingSendsJobWideCancelOnlyForSoleExactTask(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	runner := &scriptedRunner{results: []scriptedRunResult{
		{out: []byte(`[{"job_id":42,"task_id":"task-a","status":"RUNNING"}]`)},
		{out: []byte("cancel requested")},
	}}

	requested, err := (Client{Runner: runner}).CancelBinding(context.Background(), database, binding)
	if err != nil || !requested {
		t.Fatalf("CancelBinding = %t, %v", requested, err)
	}
	if runner.calls != 2 || strings.Join(runner.args[1], " ") != "jobs cancel -y 42" {
		t.Fatalf("runner calls/args = %d/%v, want scoped observation then job cancel", runner.calls, runner.args)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if job.Status != db.StatusRunning || refreshed.CancelRequestedAt == nil {
		t.Fatalf("status/intent = %q/%v, want running with secondary cancel intent", job.Status, refreshed.CancelRequestedAt)
	}
}

func TestCancelBindingRecordsFreshTerminalObservationWithoutCanceling(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	runner := &scriptedRunner{results: []scriptedRunResult{{out: []byte(`[
		{"job_id":42,"task_id":"task-a","status":"SUCCEEDED"}
	]`)}}}

	requested, err := (Client{Runner: runner}).CancelBinding(context.Background(), database, binding)
	if err != nil || requested {
		t.Fatalf("CancelBinding = %t, %v; want terminal observation without cancel", requested, err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want queue observation only", runner.calls)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if job.Status != db.StatusCompleted || refreshed.CancelRequestedAt != nil {
		t.Fatalf("status/intent = %q/%v, want completed without cancel intent", job.Status, refreshed.CancelRequestedAt)
	}
}
