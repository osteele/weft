package terminal

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/skypilot"
)

type cancelAwareSkyRunner struct {
	started  chan struct{}
	release  chan struct{}
	canceled atomic.Bool
}

func (r *cancelAwareSkyRunner) Run(ctx context.Context, _ ...string) ([]byte, error) {
	close(r.started)
	select {
	case <-ctx.Done():
		r.canceled.Store(true)
		return nil, ctx.Err()
	case <-r.release:
		return []byte(`[]`), nil
	}
}

type tuiSkyRunner struct {
	out []byte
}

func (r tuiSkyRunner) Run(context.Context, ...string) ([]byte, error) {
	return r.out, nil
}

type recordingTUISkyRunner struct {
	args [][]string
}

func (r *recordingTUISkyRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.args = append(r.args, append([]string(nil), args...))
	if len(args) >= 2 && args[0] == "jobs" && args[1] == "queue" {
		return []byte(`[{"job_id":42,"task_id":"task-a","status":"RUNNING"}]`), nil
	}
	return []byte("cancel requested"), nil
}

func TestSyncExternalStateForTUIRefreshesSkyPilotJob(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor:         db.ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "RUNNING",
		NormalizedStatus: db.StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create SkyPilot binding: %v", err)
	}
	previous := skyClientForTUI
	skyClientForTUI = skypilot.Client{Runner: tuiSkyRunner{out: []byte(`[
		{"job_id":42,"status":"SUCCEEDED"}
	]`)}}
	t.Cleanup(func() { skyClientForTUI = previous })

	if warnings := syncExternalStateForTUI(context.Background(), database); len(warnings) != 0 {
		t.Fatalf("syncExternalStateForTUI warnings = %v", warnings)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get refreshed job: %v", err)
	}
	if job.Status != db.StatusCompleted || job.EndTime == nil {
		t.Fatalf("status, end_time = %q, %v; want completed with end time", job.Status, job.EndTime)
	}
}

func TestWatchedJobRefreshIncludesSkyPilot(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create SkyPilot binding: %v", err)
	}
	previous := skyClientForTUI
	skyClientForTUI = skypilot.Client{Runner: tuiSkyRunner{out: []byte(`[{"job_id":42,"status":"SUCCEEDED"}]`)}}
	t.Cleanup(func() { skyClientForTUI = previous })

	if warnings := syncWatchedJobStateQuiet(context.Background(), database, []int64{binding.JobID}); len(warnings) != 0 {
		t.Fatalf("watch sync warnings = %v", warnings)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get watched job: %v", err)
	}
	if job.Status != db.StatusCompleted {
		t.Fatalf("watched job status = %q, want completed", job.Status)
	}
}

func TestProjectWatchRefreshIncludesSkyPilot(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create SkyPilot binding: %v", err)
	}
	previous := skyClientForTUI
	skyClientForTUI = skypilot.Client{Runner: tuiSkyRunner{out: []byte(`[{"job_id":42,"status":"SUCCEEDED"}]`)}}
	t.Cleanup(func() { skyClientForTUI = previous })

	if warnings := syncProjectWatchData(context.Background(), database, false, false); len(warnings) != 0 {
		t.Fatalf("project watch sync warnings = %v", warnings)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get project job: %v", err)
	}
	if job.Status != db.StatusCompleted {
		t.Fatalf("project job status = %q, want completed", job.Status)
	}
}

func TestProjectWatchExternalSyncPropagatesCancellation(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	}); err != nil {
		t.Fatalf("create SkyPilot binding: %v", err)
	}
	runner := &cancelAwareSkyRunner{started: make(chan struct{}), release: make(chan struct{})}
	previous := skyClientForTUI
	skyClientForTUI = skypilot.Client{Runner: runner}
	t.Cleanup(func() { skyClientForTUI = previous })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		syncProjectWatchData(ctx, database, false, false)
		close(done)
	}()
	<-runner.started
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		close(runner.release)
		t.Fatal("project watch external sync ignored parent cancellation")
	}
	if !runner.canceled.Load() {
		t.Fatal("SkyPilot runner did not observe project-watch cancellation")
	}
}

func TestLoadExternalBindingsForVisibleJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create SkyPilot binding: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	loaded := loadExternalBindingsForJobs(database, []*db.Job{job})
	if loaded[binding.JobID] == nil || loaded[binding.JobID].ExternalJobID != "42" {
		t.Fatalf("loaded external bindings = %+v, want job %d", loaded, binding.JobID)
	}
}

func TestWatchKillRoutesQueuedAndRunningSkyPilotJobsThroughAdapter(t *testing.T) {
	for _, status := range []string{db.StatusQueued, db.StatusRunning} {
		t.Run(status, func(t *testing.T) {
			database := db.SetupTestDB(t)
			raw := "PENDING"
			if status == db.StatusRunning {
				raw = "RUNNING"
			}
			binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
				Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
				RawStatus: raw, NormalizedStatus: status, Command: "python train.py",
			})
			if err != nil {
				t.Fatalf("create external job: %v", err)
			}
			runner := &recordingTUISkyRunner{}
			previous := skyClientForTUI
			skyClientForTUI = skypilot.Client{Runner: runner}
			t.Cleanup(func() { skyClientForTUI = previous })

			msg := requestWatchJobKill(context.Background(), database, binding.JobID)()
			result, ok := msg.(watchKillDoneMsg)
			if !ok || result.err != nil {
				t.Fatalf("watch kill result = %#v", msg)
			}
			if len(runner.args) != 2 {
				t.Fatalf("SkyPilot calls = %v, want queue observation and cancel", runner.args)
			}
			job, err := db.GetJobByID(database, binding.JobID)
			if err != nil {
				t.Fatalf("get job: %v", err)
			}
			refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
			if err != nil {
				t.Fatalf("get binding: %v", err)
			}
			if job.Status != status || refreshed.CancelRequestedAt == nil {
				t.Fatalf("status/intent = %q/%v, want %q with pending cancel", job.Status, refreshed.CancelRequestedAt, status)
			}
		})
	}
}
