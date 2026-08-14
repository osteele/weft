package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/skypilot"
	"github.com/spf13/cobra"
)

type terminalizingCancelRunner struct {
	database *sql.DB
	jobID    int64
}

func (r terminalizingCancelRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "jobs" && args[1] == "queue" {
		return []byte(`[{"job_id":42,"status":"RUNNING"}]`), nil
	}
	err := db.UpdateExternalJobObservation(r.database, r.jobID, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "SUCCEEDED", NormalizedStatus: db.StatusCompleted,
	})
	return nil, err
}

type inspectingCancelRunner struct {
	database  *sql.DB
	jobID     int64
	sawIntent bool
}

func (r *inspectingCancelRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "jobs" && args[1] == "queue" {
		return []byte(`[{"job_id":42,"status":"RUNNING"}]`), nil
	}
	var requested sql.NullInt64
	if err := r.database.QueryRow(`SELECT cancel_requested_at FROM external_job_bindings WHERE job_id = ?`, r.jobID).Scan(&requested); err != nil {
		return nil, err
	}
	r.sawIntent = requested.Valid
	return nil, nil
}

type skyTestRunner struct {
	results []skyTestRunResult
	calls   int
	args    [][]string
}

type skyTestRunResult struct {
	out []byte
	err error
}

type deadlineCapturingSkyRunner struct {
	deadline time.Time
}

func (r *deadlineCapturingSkyRunner) Run(ctx context.Context, _ ...string) ([]byte, error) {
	r.deadline, _ = ctx.Deadline()
	return []byte(`[{"job_id":42,"status":"RUNNING"}]`), nil
}

type blockingSkyRunner struct {
	calls int
}

func (r *blockingSkyRunner) Run(ctx context.Context, _ ...string) ([]byte, error) {
	r.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *skyTestRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.args = append(r.args, append([]string(nil), args...))
	if r.calls >= len(r.results) {
		return nil, fmt.Errorf("unexpected SkyPilot CLI call %d", r.calls+1)
	}
	result := r.results[r.calls]
	r.calls++
	return result.out, result.err
}

func (r *skyTestRunner) RunStreaming(_ context.Context, stdout, _ io.Writer, args ...string) error {
	out, err := r.Run(context.Background(), args...)
	if len(out) > 0 {
		if _, writeErr := stdout.Write(out); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func TestSkyLogPushesTailBoundToExecutor(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	for _, tc := range []struct {
		name string
		full bool
		want string
	}{
		{name: "default tail", want: "jobs logs 42 task-a --no-follow --tail 50"},
		{name: "full", full: true, want: "jobs logs 42 task-a --no-follow --tail 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetLogModeState()
			defer resetLogModeState()
			if tc.full {
				logFull = true
				logFrom = 1
			}
			runner := &skyTestRunner{results: []skyTestRunResult{{out: []byte("line\n")}}}
			useSkyTestClient(t, runner)
			captureStdout(t, func() {
				if err := runSkyLogForJob(skyTestCommand(), database, job); err != nil {
					t.Fatalf("runSkyLogForJob: %v", err)
				}
			})
			if got := strings.Join(runner.args[0], " "); got != tc.want {
				t.Fatalf("SkyPilot log args = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSkyLogAttemptSelectionNeverSubstitutesAnotherAttempt(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	resetLogModeState()
	defer resetLogModeState()
	runner := &skyTestRunner{results: []skyTestRunResult{{out: []byte("current attempt\n")}}}
	useSkyTestClient(t, runner)

	logAttempt = 99
	if err := runLogForJob(skyTestCommand(), database, binding.JobID); err == nil || !strings.Contains(err.Error(), "no attempt #99") {
		t.Fatalf("missing attempt error = %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("missing attempt made %d SkyPilot calls", runner.calls)
	}

	attempts, err := db.ListAttempts(database, binding.JobID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("list attempts = %+v, %v", attempts, err)
	}
	logAttempt = attempts[0].AttemptNumber
	cmd := skyTestCommand()
	var stdout strings.Builder
	cmd.SetOut(&stdout)
	if err := runLogForJob(cmd, database, binding.JobID); err != nil {
		t.Fatalf("bound attempt log: %v", err)
	}
	if got, want := strings.Join(runner.args[0], " "), "jobs logs 42 task-a --no-follow --tail 50"; got != want {
		t.Fatalf("bound attempt args = %q, want %q", got, want)
	}
	if stdout.String() != "current attempt\n" {
		t.Fatalf("bound attempt output = %q", stdout.String())
	}
}

func TestSkyFollowAllowsExplicitTailAndStreamsToCommandOutput(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	resetLogModeState()
	defer resetLogModeState()
	logFollow = true
	logLines = 100
	runner := &skyTestRunner{results: []skyTestRunResult{{out: []byte("live line\n")}}}
	useSkyTestClient(t, runner)
	cmd := skyTestCommand()
	var stdout strings.Builder
	cmd.SetOut(&stdout)
	if err := runSkyLogForJob(cmd, database, job); err != nil {
		t.Fatalf("runSkyLogForJob: %v", err)
	}
	if got, want := strings.Join(runner.args[0], " "), "jobs logs 42 task-a --follow --tail 100"; got != want {
		t.Fatalf("SkyPilot follow args = %q, want %q", got, want)
	}
	if got := stdout.String(); got != "live line\n" {
		t.Fatalf("command output = %q", got)
	}
}

func TestSkyFollowStreamsGrepFilter(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	resetLogModeState()
	defer resetLogModeState()
	logFollow = true
	logLines = 2
	logGrep = `epoch [0-9]+`
	// SkyPilot applies --tail before emitting the retained portion of a follow
	// stream, so the fixture contains only the requested two-line window.
	runner := &skyTestRunner{results: []skyTestRunResult{{out: []byte("noise\nepoch 2\n")}}}
	useSkyTestClient(t, runner)
	cmd := skyTestCommand()
	var stdout strings.Builder
	cmd.SetOut(&stdout)
	if err := runSkyLogForJob(cmd, database, job); err != nil {
		t.Fatalf("runSkyLogForJob: %v", err)
	}
	if got, want := strings.Join(runner.args[0], " "), "jobs logs 42 task-a --follow --tail 2"; got != want {
		t.Fatalf("SkyPilot filtered-follow args = %q, want %q", got, want)
	}
	if got, want := stdout.String(), "epoch 2\n"; got != want {
		t.Fatalf("filtered follow output = %q, want %q", got, want)
	}
}

func TestSkyFollowRejectsZeroTailWithoutDownloadingRetainedLog(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	resetLogModeState()
	defer resetLogModeState()
	logFollow = true
	logLines = 0
	runner := &skyTestRunner{}
	useSkyTestClient(t, runner)
	if err := runSkyLogForJob(skyTestCommand(), database, job); err == nil || !strings.Contains(err.Error(), "requires a positive") {
		t.Fatalf("zero-tail follow error = %v", err)
	}
	if len(runner.args) != 0 {
		t.Fatalf("zero-tail follow called SkyPilot with %v", runner.args)
	}
}

func TestSkyZeroLineSnapshotDoesNotDownloadRetainedLog(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	resetLogModeState()
	defer resetLogModeState()
	logLines = 0
	runner := &skyTestRunner{}
	useSkyTestClient(t, runner)
	if err := runSkyLogForJob(skyTestCommand(), database, job); err != nil {
		t.Fatalf("zero-line snapshot: %v", err)
	}
	if len(runner.args) != 0 {
		t.Fatalf("zero-line snapshot called SkyPilot with %v", runner.args)
	}
}

func TestStreamingLogFilterBoundsUnterminatedLine(t *testing.T) {
	filter := newStreamingLogFilter(io.Discard, 0, "match")
	chunk := make([]byte, maxStreamingLogLineBytes+1)
	if _, err := filter.Write(chunk); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized streaming line error = %v", err)
	}
}

func useSkyTestClient(t *testing.T, runner skypilot.Runner) {
	t.Helper()
	previous := skyClient
	skyClient = skypilot.Client{Runner: runner}
	t.Cleanup(func() { skyClient = previous })
}

func TestListSkySyncUsesFiveSecondObservationBudget(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	runner := &deadlineCapturingSkyRunner{}
	useSkyTestClient(t, runner)
	started := time.Now()
	if warnings := syncSkyForList(context.Background(), database); len(warnings) != 0 {
		t.Fatalf("sync warnings = %v", warnings)
	}
	budget := runner.deadline.Sub(started)
	if budget <= 0 || budget > 6*time.Second {
		t.Fatalf("list Sky sync deadline budget = %s, want about 5s", budget)
	}
}

func TestStatusSyncAndWaitRefreshSkyPilotWithoutGenericSSH(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create running external job: %v", err)
	}
	restoreStatusFlags(t)
	statusSync = true
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"task_id":"task-a","status":"SUCCEEDED"}
	]`)}}})
	cmd := skyTestCommand()
	captureStdout(t, func() {
		if err := runStatus(cmd, []string{fmt.Sprint(binding.JobID)}); err != nil {
			t.Fatalf("status --sync: %v", err)
		}
	})
	completed, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get refreshed job: %v", err)
	}
	if completed.Status != db.StatusCompleted || completed.ExitCode != nil {
		t.Fatalf("refreshed status/exit = %q/%v, want completed with executor-owned nil exit", completed.Status, completed.ExitCode)
	}
	requests := []jobStatusRequest{{ID: binding.JobID, Job: completed}}
	if !allJobsSucceeded(requests, map[int64]*db.Job{binding.JobID: completed}) {
		t.Fatal("completed SkyPilot job with nil exit code was treated as failed")
	}

	waitBinding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "43", ExternalTaskID: "task-b",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python eval.py",
	})
	if err != nil {
		t.Fatalf("create wait target: %v", err)
	}
	running, err := db.GetJobByID(database, waitBinding.JobID)
	if err != nil {
		t.Fatalf("get running job: %v", err)
	}
	runner := &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":43,"task_id":"task-b","status":"SUCCEEDED"}
	]`)}}}
	useSkyTestClient(t, runner)
	originalGenericSync := syncJobForStatusWaitFunc
	genericSyncCalls := 0
	syncJobForStatusWaitFunc = func(_ *sql.DB, _ *db.Job, _ ops.SyncOptions) (ops.SyncResult, error) {
		genericSyncCalls++
		return ops.SyncResult{}, errors.New("generic SSH sync must not run for SkyPilot")
	}
	t.Cleanup(func() { syncJobForStatusWaitFunc = originalGenericSync })
	final, err := waitForJobsCompletion(context.Background(), database,
		[]jobStatusRequest{{ID: waitBinding.JobID, Job: running}}, time.Second, newHostConnectionTracker())
	if err != nil {
		t.Fatalf("wait for external job: %v", err)
	}
	if final[waitBinding.JobID] == nil || final[waitBinding.JobID].Status != db.StatusCompleted || genericSyncCalls != 0 {
		t.Fatalf("wait result/generic calls = %+v/%d", final[waitBinding.JobID], genericSyncCalls)
	}
	untargeted, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get untargeted binding: %v", err)
	}
	if untargeted.SyncWarning != "" {
		t.Fatalf("targeted wait mutated another binding warning: %q", untargeted.SyncWarning)
	}
}

func TestStatusWaitRefreshesMultipleSkyPilotJobsWithOneQueueSnapshot(t *testing.T) {
	database := db.SetupTestDB(t)
	var requests []jobStatusRequest
	for _, externalID := range []string{"42", "43"} {
		binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
			Executor: db.ExternalExecutorSkyPilot, ExternalJobID: externalID,
			RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
		})
		if err != nil {
			t.Fatalf("create external job %s: %v", externalID, err)
		}
		job, err := db.GetJobByID(database, binding.JobID)
		if err != nil {
			t.Fatalf("get external job %s: %v", externalID, err)
		}
		requests = append(requests, jobStatusRequest{ID: binding.JobID, Job: job})
	}
	runner := &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"status":"SUCCEEDED"},
		{"job_id":43,"status":"SUCCEEDED"}
	]`)}}}
	useSkyTestClient(t, runner)
	final, err := waitForJobsCompletionPolling(context.Background(), database, map[int64]*db.Job{},
		map[int64]struct{}{requests[0].ID: {}, requests[1].ID: {}},
		[]int64{requests[0].ID, requests[1].ID}, map[int64]string{}, time.Second, newHostConnectionTracker())
	if err != nil {
		t.Fatalf("wait for two external jobs: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("SkyPilot queue calls = %d, want one snapshot for both jobs", runner.calls)
	}
	for _, req := range requests {
		if final[req.ID] == nil || final[req.ID].Status != db.StatusCompleted {
			t.Fatalf("final %s = %+v, want completed", ids.FormatJobID(req.ID), final[req.ID])
		}
	}
}

func TestStatusWaitBoundsSkyPilotSnapshotByWaitTimeout(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	runner := &blockingSkyRunner{}
	useSkyTestClient(t, runner)
	started := time.Now()
	_, err = waitForJobsCompletionPolling(context.Background(), database, map[int64]*db.Job{},
		map[int64]struct{}{binding.JobID: {}}, []int64{binding.JobID}, map[int64]string{},
		100*time.Millisecond, newHostConnectionTracker())
	if !errors.Is(err, errWaitTimeout) {
		t.Fatalf("wait error = %v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("100ms wait took %s", elapsed)
	}
	if runner.calls != 1 {
		t.Fatalf("SkyPilot queue calls = %d, want one bounded call", runner.calls)
	}
}

func TestExternalStatusRefreshFailurePreservesPositiveStateWithWarning(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create running external job: %v", err)
	}
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{err: errors.New("queue timeout")}}})
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get running job: %v", err)
	}
	if err := syncExternalStatusJobs(context.Background(), database, []*db.Job{job}); err == nil {
		t.Fatal("failed external observation returned nil error")
	}
	after, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get preserved job: %v", err)
	}
	refreshedBinding, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get warned binding: %v", err)
	}
	if after.Status != db.StatusRunning || refreshedBinding.SyncWarning == "" {
		t.Fatalf("status/warning = %q/%q, want preserved running state with warning", after.Status, refreshedBinding.SyncWarning)
	}
}

func TestAllJobsSucceededRejectsExternalFailureStates(t *testing.T) {
	for _, status := range []string{db.StatusFailed, db.StatusCanceled, db.StatusDead} {
		job := &db.Job{ID: 42, Backend: db.BackendSkyPilot, Status: status}
		if allJobsSucceeded([]jobStatusRequest{{ID: job.ID, Job: job}}, map[int64]*db.Job{job.ID: job}) {
			t.Fatalf("external status %q was treated as successful", status)
		}
	}
}

func resetSkySubmitFlags(t *testing.T) {
	t.Helper()
	project, cwd, description := skySubmitProject, skySubmitCWD, skySubmitDescription
	gpu, gpuClass := skySubmitGPU, skySubmitGPUClass
	gpuCount, gpuMem := skySubmitGPUCount, skySubmitGPUMem
	env, tags, dryRun := skySubmitEnv, skySubmitTags, skySubmitDryRun
	skySubmitProject = ""
	skySubmitCWD = ""
	skySubmitDescription = ""
	skySubmitGPU = ""
	skySubmitGPUClass = ""
	skySubmitGPUCount = 1
	skySubmitGPUMem = 0
	skySubmitEnv = nil
	skySubmitTags = nil
	skySubmitDryRun = ""
	t.Cleanup(func() {
		skySubmitProject, skySubmitCWD, skySubmitDescription = project, cwd, description
		skySubmitGPU, skySubmitGPUClass = gpu, gpuClass
		skySubmitGPUCount, skySubmitGPUMem = gpuCount, gpuMem
		skySubmitEnv, skySubmitTags, skySubmitDryRun = env, tags, dryRun
	})
}

func skyTestCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	return cmd
}

func TestSkySubmitUnknownOutcomeRemainsNonterminal(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := database.Close(); err != nil {
		t.Fatalf("close setup database: %v", err)
	}
	resetSkySubmitFlags(t)
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{err: context.DeadlineExceeded}}})

	err := runSkySubmit(skyTestCommand(), []string{"python train.py"})
	if err == nil {
		t.Fatal("runSkySubmit returned nil error, want unconfirmed submission")
	}
	var unconfirmed *skypilot.SubmissionUnconfirmedError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("runSkySubmit error = %T %v, want SubmissionUnconfirmedError", err, err)
	}

	inspect, err := db.Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer inspect.Close()
	jobs, err := db.ListJobsByStatuses(inspect, nil, "", "", 0, nil, "")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if jobs[0].Status != db.StatusQueued || db.IsTerminalStatus(jobs[0].Status) {
		t.Fatalf("status = %q, want queued and nonterminal", jobs[0].Status)
	}
	binding, err := db.GetExternalJobBindingByJobID(inspect, jobs[0].ID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if binding != nil {
		t.Fatalf("binding = %+v, want nil for unresolved identity", binding)
	}
}

func TestSkySubmitDefinitiveFailureClosesAttempt(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := database.Close(); err != nil {
		t.Fatalf("close setup database: %v", err)
	}
	resetSkySubmitFlags(t)
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{err: &skypilot.SubmissionRefusedError{Cause: fmt.Errorf("quota refused")}}}})

	err := runSkySubmit(skyTestCommand(), []string{"python train.py"})
	if err == nil || !strings.Contains(err.Error(), "quota refused") {
		t.Fatalf("runSkySubmit error = %v, want quota refusal", err)
	}

	inspect, err := db.Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer inspect.Close()
	jobs, err := db.ListJobsByStatuses(inspect, nil, "", "", 0, nil, "")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != db.StatusDead {
		t.Fatalf("jobs = %+v, want one dead job", jobs)
	}
}

func TestSkySubmitRejectsUnsupportedGPUMemoryBeforeLedgerWrite(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := database.Close(); err != nil {
		t.Fatalf("close setup database: %v", err)
	}
	resetSkySubmitFlags(t)
	skySubmitGPUMem = 80
	runner := &skyTestRunner{}
	useSkyTestClient(t, runner)

	err := runSkySubmit(skyTestCommand(), []string{"python train.py"})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("runSkySubmit error = %v, want unsupported GPU memory", err)
	}
	if runner.calls != 0 {
		t.Fatalf("SkyPilot runner calls = %d, want 0", runner.calls)
	}
	inspect, err := db.Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer inspect.Close()
	var jobs int
	if err := inspect.QueryRow(`SELECT COUNT(*) FROM jobs WHERE backend = ?`, db.BackendSkyPilot).Scan(&jobs); err != nil {
		t.Fatalf("count SkyPilot jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("SkyPilot job count = %d, want 0 after local validation failure", jobs)
	}
}

func TestSkySyncEmptyStatusLeavesLastObservationAlone(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor:         db.ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "RUNNING",
		RawStatusMessage: "healthy",
		NormalizedStatus: db.StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	before := *binding.LastObservedAt
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[{"job_id":42,"name":"weft-wj1","status":"","message":"partial row"}]`)}}})

	updated, _, err := syncSkyBindings(context.Background(), database, "")
	if err != nil {
		t.Fatalf("syncSkyBindings: %v", err)
	}
	if updated != 0 {
		t.Fatalf("updated = %d, want 0 for an observation without status", updated)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusRunning {
		t.Fatalf("status = %q, want last confirmed running status", job.Status)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get refreshed binding: %v", err)
	}
	if refreshed.RawStatus != "RUNNING" || refreshed.RawStatusMessage != "healthy" {
		t.Fatalf("last observation changed to raw=%q message=%q", refreshed.RawStatus, refreshed.RawStatusMessage)
	}
	if refreshed.LastObservedAt == nil || *refreshed.LastObservedAt != before {
		t.Fatalf("last_observed_at = %v, want unchanged %d", refreshed.LastObservedAt, before)
	}
	if refreshed.SyncWarning == "" {
		t.Fatal("missing sync warning for row without status")
	}
}

func TestSkyImportTaskPersistsCompleteIdentity(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := database.Close(); err != nil {
		t.Fatalf("close setup database: %v", err)
	}
	previousProject, previousCWD, previousJob, previousTask, previousCommand := skyImportProject, skyImportCWD, skyImportJob, skyImportTask, skyImportCommand
	skyImportProject, skyImportCWD, skyImportJob, skyImportTask, skyImportCommand = "", "", "", "train", "python train.py"
	t.Cleanup(func() {
		skyImportProject, skyImportCWD, skyImportJob, skyImportTask, skyImportCommand = previousProject, previousCWD, previousJob, previousTask, previousCommand
	})
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"task_id":0,"task_name":"train","status":"RUNNING"},
		{"job_id":42,"task_id":1,"task_name":"eval","status":"RUNNING"},
		{"job_id":99,"task_id":0,"task_name":"train","status":"RUNNING"}
	]`)}}})
	if err := runSkyImport(skyTestCommand(), []string{"42"}); err != nil {
		t.Fatalf("runSkyImport: %v", err)
	}
	inspect, err := db.Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer inspect.Close()
	bindings, err := db.ListExternalJobBindings(inspect, db.ExternalExecutorSkyPilot, "")
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].ExternalJobID != "42" || bindings[0].ExternalTaskID != "0" {
		t.Fatalf("bindings = %+v, want one complete 42/0 identity", bindings)
	}
	job, err := db.GetJobByID(inspect, bindings[0].JobID)
	if err != nil || job == nil || job.Command != "python train.py" {
		t.Fatalf("imported command job = %+v, err %v", job, err)
	}
}

func TestSkyImportRequiresCommandWhenCurrentQueueOmitsIt(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := database.Close(); err != nil {
		t.Fatalf("close setup database: %v", err)
	}
	previousProject, previousCWD, previousJob, previousTask, previousCommand := skyImportProject, skyImportCWD, skyImportJob, skyImportTask, skyImportCommand
	skyImportProject, skyImportCWD, skyImportJob, skyImportTask, skyImportCommand = "", "", "", "", ""
	t.Cleanup(func() {
		skyImportProject, skyImportCWD, skyImportJob, skyImportTask, skyImportCommand = previousProject, previousCWD, previousJob, previousTask, previousCommand
	})
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"task_id":0,"task_name":"train","status":"RUNNING"}
	]`)}}})
	err := runSkyImport(skyTestCommand(), []string{"42"})
	if err == nil || !strings.Contains(err.Error(), "pass --command") {
		t.Fatalf("runSkyImport error = %v, want actionable command requirement", err)
	}
	inspect, err := db.Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer inspect.Close()
	var count int
	if err := inspect.QueryRow(`SELECT COUNT(*) FROM jobs WHERE backend = ?`, db.BackendSkyPilot).Scan(&count); err != nil {
		t.Fatalf("count external jobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("external job count = %d, want no placeholder import", count)
	}
}

func TestSkySyncDoesNotAliasJobIDWithAnotherJobName(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor:         db.ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "RUNNING",
		RawStatusMessage: "actual job",
		NormalizedStatus: db.StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"name":"weft-wj1","status":"RUNNING","message":"actual job"},
		{"job_id":99,"name":"42","status":"SUCCEEDED","message":"different job"}
	]`)}}})

	updated, missing, err := syncSkyBindings(context.Background(), database, "")
	if err != nil {
		t.Fatalf("syncSkyBindings: %v", err)
	}
	if updated != 1 || missing != 0 {
		t.Fatalf("updated, missing = %d, %d; want 1, 0", updated, missing)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusRunning {
		t.Fatalf("status = %q, want RUNNING row selected by external job ID", job.Status)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if refreshed.RawStatusMessage != "actual job" {
		t.Fatalf("raw status message = %q, want actual job", refreshed.RawStatusMessage)
	}
}

func TestSkySyncUsesStoredJobAndTaskIdentityDespiteNonUniqueName(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor:         db.ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		ExternalTaskID:   "task-a",
		RawStatus:        "RUNNING",
		RawStatusMessage: "actual task",
		NormalizedStatus: db.StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"task_id":"task-a","name":"shared-name","status":"RUNNING","message":"actual task"},
		{"job_id":99,"task_id":"task-b","name":"shared-name","status":"SUCCEEDED","message":"different task"}
	]`)}}})

	updated, missing, err := syncSkyBindings(context.Background(), database, "")
	if err != nil {
		t.Fatalf("syncSkyBindings: %v", err)
	}
	if updated != 1 || missing != 0 {
		t.Fatalf("updated, missing = %d, %d; want 1, 0", updated, missing)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusRunning {
		t.Fatalf("status = %q, want task-a's running observation", job.Status)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if refreshed.RawStatusMessage != "actual task" {
		t.Fatalf("raw status message = %q, want actual task", refreshed.RawStatusMessage)
	}
}

func TestSkySyncMatchesFullTaskQualifiedIdentity(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor:         db.ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		ExternalTaskID:   "task-a",
		RawStatus:        "RUNNING",
		RawStatusMessage: "actual task",
		NormalizedStatus: db.StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"task_id":"task-a","status":"RUNNING","message":"actual task"},
		{"job_id":42,"task_id":"task-b","status":"SUCCEEDED","message":"different task"}
	]`)}}})

	updated, missing, err := syncSkyBindings(context.Background(), database, "")
	if err != nil {
		t.Fatalf("syncSkyBindings: %v", err)
	}
	if updated != 1 || missing != 0 {
		t.Fatalf("updated, missing = %d, %d; want 1, 0", updated, missing)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusRunning {
		t.Fatalf("status = %q, want task-a's running observation", job.Status)
	}
}

func TestAmbientSkySyncNotConfiguredTouchesNothing(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor:         db.ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "RUNNING",
		RawStatusMessage: "healthy",
		NormalizedStatus: db.StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	before := *binding.LastObservedAt
	previous := skyClient
	skyClient = skypilot.Client{LookPath: func(string) (string, error) {
		return "", fmt.Errorf("executable not found")
	}}
	t.Cleanup(func() { skyClient = previous })

	updated, missing, err := syncSkyBindingsAmbient(context.Background(), database, "")
	if err != nil {
		t.Fatalf("syncSkyBindingsAmbient: %v", err)
	}
	if updated != 0 || missing != 0 {
		t.Fatalf("updated, missing = %d, %d; want 0, 0", updated, missing)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if refreshed.SyncWarning != "" || refreshed.SyncWarningAt != nil {
		t.Fatalf("not-configured ambient sync wrote warning %q at %v", refreshed.SyncWarning, refreshed.SyncWarningAt)
	}
	if refreshed.LastObservedAt == nil || *refreshed.LastObservedAt != before {
		t.Fatalf("last_observed_at = %v, want unchanged %d", refreshed.LastObservedAt, before)
	}
}

func TestExplicitSkySyncNotConfiguredReturnsErrorWithoutMutation(t *testing.T) {
	database := db.SetupTestDB(t)
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
	previous := skyClient
	skyClient = skypilot.Client{LookPath: func(string) (string, error) {
		return "", fmt.Errorf("executable not found")
	}}
	t.Cleanup(func() { skyClient = previous })

	_, _, err = syncSkyBindings(context.Background(), database, "")
	if !errors.Is(err, skypilot.ErrNotConfigured) {
		t.Fatalf("syncSkyBindings error = %v, want ErrNotConfigured", err)
	}
	refreshed, getErr := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if getErr != nil {
		t.Fatalf("get binding: %v", getErr)
	}
	if refreshed.SyncWarning != "" || refreshed.SyncWarningAt != nil {
		t.Fatalf("not-configured explicit sync wrote warning %q at %v", refreshed.SyncWarning, refreshed.SyncWarningAt)
	}
}

func TestSkySyncQueryFailurePreservesEveryBindingWithOneProbe(t *testing.T) {
	database := db.SetupTestDB(t)
	type priorObservation struct {
		jobID          int64
		lastObservedAt int64
	}
	var prior []priorObservation
	for _, externalID := range []string{"42", "43"} {
		binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
			Executor:         db.ExternalExecutorSkyPilot,
			ExternalJobID:    externalID,
			RawStatus:        "RUNNING",
			RawStatusMessage: "healthy " + externalID,
			NormalizedStatus: db.StatusRunning,
			Command:          "python train.py",
		})
		if err != nil {
			t.Fatalf("create binding %s: %v", externalID, err)
		}
		prior = append(prior, priorObservation{jobID: binding.JobID, lastObservedAt: *binding.LastObservedAt})
	}
	runner := &skyTestRunner{results: []skyTestRunResult{{err: fmt.Errorf("executor unavailable")}}}
	useSkyTestClient(t, runner)

	updated, missing, err := syncSkyBindings(context.Background(), database, "")
	if err == nil || !strings.Contains(err.Error(), "executor unavailable") {
		t.Fatalf("syncSkyBindings error = %v, want executor unavailable", err)
	}
	if updated != 0 || missing != 0 {
		t.Fatalf("updated, missing = %d, %d; want 0, 0", updated, missing)
	}
	if runner.calls != 1 {
		t.Fatalf("SkyPilot queue probes = %d, want one per refresh pass", runner.calls)
	}
	for _, before := range prior {
		job, err := db.GetJobByID(database, before.jobID)
		if err != nil {
			t.Fatalf("get job %d: %v", before.jobID, err)
		}
		if job.Status != db.StatusRunning {
			t.Fatalf("job %d status = %q, want running", before.jobID, job.Status)
		}
		binding, err := db.GetExternalJobBindingByJobID(database, before.jobID)
		if err != nil {
			t.Fatalf("get binding %d: %v", before.jobID, err)
		}
		if binding.LastObservedAt == nil || *binding.LastObservedAt != before.lastObservedAt {
			t.Fatalf("job %d last_observed_at = %v, want unchanged %d", before.jobID, binding.LastObservedAt, before.lastObservedAt)
		}
		if binding.SyncWarning == "" {
			t.Fatalf("job %d missing sync warning", before.jobID)
		}
	}
}

func TestSkySyncPositiveTerminalObservationClosesJob(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor:         db.ExternalExecutorSkyPilot,
		ExternalJobID:    "42",
		RawStatus:        "RUNNING",
		RawStatusMessage: "healthy",
		NormalizedStatus: db.StatusRunning,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"status":"SUCCEEDED","message":"done"}
	]`)}}})

	updated, missing, err := syncSkyBindings(context.Background(), database, "")
	if err != nil {
		t.Fatalf("syncSkyBindings: %v", err)
	}
	if updated != 1 || missing != 0 {
		t.Fatalf("updated, missing = %d, %d; want 1, 0", updated, missing)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusCompleted || job.EndTime == nil {
		t.Fatalf("status, end_time = %q, %v; want completed with end time", job.Status, job.EndTime)
	}
}

func TestSkyImportRebindsExplicitUnconfirmedJob(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _, err := db.CreateExternalPendingJob(database, db.ExternalJobObservation{
		Executor:         db.ExternalExecutorSkyPilot,
		RawStatus:        "submitted",
		NormalizedStatus: db.StatusQueued,
		Command:          "python train.py",
	})
	if err != nil {
		t.Fatalf("create unconfirmed job: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close setup database: %v", err)
	}
	previousImportJob := skyImportJob
	skyImportJob = ids.FormatJobID(jobID)
	t.Cleanup(func() { skyImportJob = previousImportJob })
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"task_id":"task-a","name":"weft-wj1","status":"RUNNING"}
	]`)}}})

	if err := runSkyImport(skyTestCommand(), []string{"42"}); err != nil {
		t.Fatalf("runSkyImport: %v", err)
	}
	inspect, err := db.Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer inspect.Close()
	var jobs int
	if err := inspect.QueryRow(`SELECT COUNT(*) FROM jobs WHERE backend = ?`, db.BackendSkyPilot).Scan(&jobs); err != nil {
		t.Fatalf("count SkyPilot jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("SkyPilot job count = %d, want original job only", jobs)
	}
	binding, err := db.GetExternalJobBindingByJobID(inspect, jobID)
	if err != nil {
		t.Fatalf("get rebound binding: %v", err)
	}
	if binding == nil || binding.ExternalJobID != "42" || binding.ExternalTaskID != "task-a" {
		t.Fatalf("rebound binding = %+v, want job 42/task-a", binding)
	}
}

func TestSkyImportTaskRefinesProvisionalBindingForMultiTaskJob(t *testing.T) {
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
		t.Fatalf("attach provisional binding: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close setup database: %v", err)
	}
	previousJob, previousTask := skyImportJob, skyImportTask
	skyImportJob, skyImportTask = ids.FormatJobID(jobID), "train"
	t.Cleanup(func() { skyImportJob, skyImportTask = previousJob, previousTask })
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{{out: []byte(`[
		{"job_id":42,"task_id":0,"task_name":"train","status":"RUNNING"},
		{"job_id":42,"task_id":1,"task_name":"eval","status":"RUNNING"}
	]`)}}})

	if err := runSkyImport(skyTestCommand(), []string{"42"}); err != nil {
		t.Fatalf("runSkyImport: %v", err)
	}
	inspect, err := db.Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer inspect.Close()
	binding, err := db.GetExternalJobBindingByJobID(inspect, jobID)
	if err != nil {
		t.Fatalf("get refined binding: %v", err)
	}
	var count int
	if err := inspect.QueryRow(`SELECT COUNT(*) FROM external_job_bindings WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if count != 1 || binding == nil || binding.ExternalTaskID != "0" {
		t.Fatalf("refined binding = count %d, %+v; want one 42/0 binding", count, binding)
	}
}

func TestSkyCancelCannotMaskTerminalObservationThatRacesWithRPC(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create running job: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	useSkyTestClient(t, terminalizingCancelRunner{database: database, jobID: binding.JobID})
	if handled, err := cancelSkyJob(skyTestCommand(), database, job); err != nil || !handled {
		t.Fatalf("cancelSkyJob = %t, %v", handled, err)
	}
	after, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	if after.Status != db.StatusCompleted {
		t.Fatalf("status = %q, want completed terminal observation", after.Status)
	}
	var requested sql.NullString
	if err := database.QueryRow(`SELECT requested_status FROM jobs WHERE id = ?`, binding.JobID).Scan(&requested); err != nil {
		t.Fatalf("read requested status: %v", err)
	}
	if requested.Valid {
		t.Fatalf("requested_status = %q, want null", requested.String)
	}
}

func TestSkyCancelPersistsIntentOnlyAfterRemoteRequestReturns(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create running job: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	runner := &inspectingCancelRunner{database: database, jobID: binding.JobID}
	useSkyTestClient(t, runner)
	if handled, err := cancelSkyJob(skyTestCommand(), database, job); err != nil || !handled {
		t.Fatalf("cancelSkyJob = %t, %v", handled, err)
	}
	if runner.sawIntent {
		t.Fatal("remote cancel observed local intent before request returned")
	}
	after, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job after cancel request: %v", err)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding after cancel request: %v", err)
	}
	if after.Status != db.StatusRunning || refreshed.CancelRequestedAt == nil {
		t.Fatalf("status/intent = %q/%v, want running with pending external cancel", after.Status, refreshed.CancelRequestedAt)
	}
}

func TestKillCommandRoutesSkyPilotJobThroughExternalCancel(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create running job: %v", err)
	}
	runner := &skyTestRunner{results: []skyTestRunResult{
		{out: []byte(`[{"job_id":42,"task_id":"task-a","status":"RUNNING"}]`)},
		{out: []byte("cancel requested")},
	}}
	useSkyTestClient(t, runner)
	cmd := skyTestCommand()
	if err := runKill(cmd, []string{ids.FormatJobID(binding.JobID)}); err != nil {
		t.Fatalf("runKill: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if job.Status != db.StatusRunning || refreshed.CancelRequestedAt == nil || runner.calls != 2 {
		t.Fatalf("status/intent/calls = %q/%v/%d", job.Status, refreshed.CancelRequestedAt, runner.calls)
	}
}

func TestSkyCancelFailureLeavesNoPendingIntent(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create running job: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	useSkyTestClient(t, &skyTestRunner{results: []skyTestRunResult{
		{out: []byte(`[{"job_id":42,"status":"RUNNING"}]`)},
		{err: errors.New("cancel refused")},
	}})
	if handled, err := cancelSkyJob(skyTestCommand(), database, job); err == nil || !handled {
		t.Fatalf("cancelSkyJob = %t, %v; want handled error", handled, err)
	}
	after, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	if after.Status != db.StatusRunning {
		t.Fatalf("status = %q, want running after failed cancel", after.Status)
	}
	var requested sql.NullString
	if err := database.QueryRow(`SELECT requested_status FROM jobs WHERE id = ?`, binding.JobID).Scan(&requested); err != nil {
		t.Fatalf("read requested status: %v", err)
	}
	if requested.Valid {
		t.Fatalf("requested_status = %q, want null after failed cancel", requested.String)
	}
	refreshed, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if refreshed.CancelRequestedAt != nil {
		t.Fatalf("cancel_requested_at = %v, want nil after failed cancel", refreshed.CancelRequestedAt)
	}
}

func TestExternalBindingDetailWarnsWhenObservationAgeAloneIsStale(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	old := now.Add(-11 * time.Minute).Unix()
	for _, tc := range []struct {
		name    string
		binding *db.ExternalJobBinding
		want    string
	}{
		{name: "never observed", binding: &db.ExternalJobBinding{ExternalJobID: "42", RawStatus: "RUNNING"}, want: "never confirmed"},
		{name: "old observation", binding: &db.ExternalJobBinding{ExternalJobID: "42", RawStatus: "RUNNING", LastObservedAt: &old}, want: "last confirmed 11m ago"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := strings.Join(externalBindingSummaryLines(nil, tc.binding, now), "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("summary missing %q:\n%s", tc.want, joined)
			}
		})
	}
}

func TestExternalBindingDetailDistinguishesUnconfirmedSubmission(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	old := &db.Job{ID: 42, Backend: db.BackendSkyPilot, Status: db.StatusQueued, CreatedAt: now.Add(-5 * time.Minute).Unix()}
	joined := strings.Join(externalBindingSummaryLines(old, nil, now), "\n")
	for _, want := range []string{"submission unconfirmed", "running or billing", "weft sky import --job wj42 <sky-job-id>"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("old unbound summary missing %q:\n%s", want, joined)
		}
	}

	young := *old
	young.CreatedAt = now.Add(-4*time.Minute - 59*time.Second).Unix()
	joined = strings.Join(externalBindingSummaryLines(&young, nil, now), "\n")
	if strings.Contains(joined, "submission unconfirmed") || !strings.Contains(joined, "binding unavailable") {
		t.Fatalf("young unbound summary = %q, want pending binding language", joined)
	}
}
