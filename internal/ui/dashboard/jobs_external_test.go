package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/monitor"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/skypilot"
)

type dashboardSkyRunner struct {
	queue []byte
	logs  []byte
	args  [][]string
}

func (r *dashboardSkyRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.args = append(r.args, append([]string(nil), args...))
	if len(args) >= 2 && args[0] == "jobs" && args[1] == "queue" {
		return r.queue, nil
	}
	if len(args) >= 2 && args[0] == "jobs" && args[1] == "logs" {
		return r.logs, nil
	}
	return []byte("cancel requested"), nil
}

func TestDashboardRestartRejectsSkyPilotBeforeLocalMutation(t *testing.T) {
	job := &db.Job{ID: 42, Backend: db.BackendSkyPilot, Status: db.StatusCompleted}
	msg := (Model{}).restartJob(job)()
	result, ok := msg.(jobRestartedMsg)
	if !ok {
		t.Fatalf("restart message = %T, want jobRestartedMsg", msg)
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "cannot be restarted") {
		t.Fatalf("restart error = %v, want external-job refusal", result.err)
	}
}

func TestDashboardExternalCancelAndUnsupportedControlsDoNotForgeTerminalState(t *testing.T) {
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
			job, err := db.GetJobByID(database, binding.JobID)
			if err != nil {
				t.Fatalf("get job: %v", err)
			}
			runner := &dashboardSkyRunner{queue: []byte(`[{"job_id":42,"task_id":"task-a","status":"RUNNING"}]`)}
			previous := skyClientForDashboard
			skyClientForDashboard = skypilot.Client{Runner: runner}
			t.Cleanup(func() { skyClientForDashboard = previous })
			model := Model{database: database, ctx: context.Background()}
			msg := model.killJob(job)()
			result, ok := msg.(jobKilledMsg)
			if !ok || result.err != nil || len(runner.args) != 2 {
				t.Fatalf("kill result/calls = %#v/%v", msg, runner.args)
			}
			refreshedJob, err := db.GetJobByID(database, binding.JobID)
			if err != nil {
				t.Fatalf("reload job: %v", err)
			}
			refreshedBinding, err := db.GetExternalJobBindingByJobID(database, binding.JobID)
			if err != nil {
				t.Fatalf("reload binding: %v", err)
			}
			if refreshedJob.Status != status || refreshedBinding.CancelRequestedAt == nil {
				t.Fatalf("status/intent = %q/%v, want %q with pending intent", refreshedJob.Status, refreshedBinding.CancelRequestedAt, status)
			}
			for name, cmd := range map[string]func() any{
				"draft":  func() any { return model.draftJob(job)() },
				"pause":  func() any { return model.pauseJob(job)() },
				"resume": func() any { return model.resumeJob(job)() },
			} {
				if cmd() == nil {
					t.Fatalf("%s returned no explicit refusal", name)
				}
			}
		})
	}
}

func TestDashboardRefreshAndLogsUseSkyPilotInsteadOfHostSSH(t *testing.T) {
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
	runner := &dashboardSkyRunner{
		queue: []byte(`[{"job_id":42,"task_id":"task-a","status":"SUCCEEDED"}]`),
		logs:  []byte("step 4/4\nfinished\n"),
	}
	previous := skyClientForDashboard
	skyClientForDashboard = skypilot.Client{Runner: runner}
	t.Cleanup(func() { skyClientForDashboard = previous })
	model := Model{database: database, ctx: context.Background(), monitor: &monitor.Monitor{}, selectedJob: job}
	if msg := model.refreshExternalJobs()(); msg == nil {
		t.Fatal("external refresh returned no message")
	}
	refreshed, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if refreshed.Status != db.StatusCompleted {
		t.Fatalf("refreshed status = %q, want completed", refreshed.Status)
	}
	msg := model.startSelectedJobLog()()
	logMsg, ok := msg.(logFetchedMsg)
	if !ok || logMsg.err != nil || !strings.Contains(logMsg.content, "finished") {
		t.Fatalf("log result = %#v", msg)
	}
	if got := strings.Join(runner.args[len(runner.args)-1], " "); got != "jobs logs 42 task-a --no-follow --tail 500" {
		t.Fatalf("log args = %q", got)
	}
}

func TestDashboardMonitorTickRefreshesExternalJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	runner := &dashboardSkyRunner{queue: []byte(`[{"job_id":42,"status":"SUCCEEDED"}]`)}
	previous := skyClientForDashboard
	skyClientForDashboard = skypilot.Client{Runner: runner}
	t.Cleanup(func() { skyClientForDashboard = previous })
	model := Model{
		database:           database,
		ctx:                context.Background(),
		monitor:            &monitor.Monitor{},
		syncActiveInterval: time.Nanosecond,
	}
	_, cmd := model.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("monitor-mode tick scheduled no work")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("monitor-mode tick message = %T, want tea.BatchMsg", cmd())
	}
	for _, child := range batch {
		_ = child()
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if job.Status != db.StatusCompleted || len(runner.args) != 1 {
		t.Fatalf("monitor refresh status/calls = %q/%v", job.Status, runner.args)
	}
}

func TestDashboardExternalCompletionWithoutExitCodeIsSucceeded(t *testing.T) {
	job := &db.Job{Backend: db.BackendSkyPilot, Status: db.StatusCompleted}
	if !jobMatchesFilter(job, jobFilterSucceeded) {
		t.Fatal("completed external job with no exit code was excluded from Succeeded")
	}
	if jobMatchesFilter(job, jobFilterFailed) {
		t.Fatal("completed external job with no exit code was included in Failed")
	}
}

func TestDashboardExternalExecutionActionsAreUnavailable(t *testing.T) {
	job := &db.Job{ID: 42, Backend: db.BackendSkyPilot, Status: db.StatusQueued, Command: "python train.py"}
	model := Model{jobs: []*db.Job{job}, jobSelectionActive: true}
	for _, keyRune := range []rune{'E', 'e', 'c'} {
		updated, _ := model.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{keyRune}})
		got := updated.(Model)
		if got.inputMode || got.showCloudMenu {
			t.Fatalf("external action %q entered input/cloud mode", keyRune)
		}
		if !got.flash.IsError {
			t.Fatalf("external action %q produced no refusal", keyRune)
		}
	}
	msg := model.launchCloudJob(job, placement.CloudOffering{})()
	result, ok := msg.(cloudJobLaunchedMsg)
	if !ok || result.err == nil || !strings.Contains(result.err.Error(), "cannot be launched") {
		t.Fatalf("launchCloudJob external result = %#v", msg)
	}
}

func TestDashboardExternalJobOmitsCPUTelemetry(t *testing.T) {
	job := &db.Job{ID: 42, Backend: db.BackendSkyPilot, Status: db.StatusRunning}
	model := Model{jobs: []*db.Job{job}, selectedJob: job, jobSelectionActive: true}
	if cmd := model.requestJobCPUTop(job); cmd != nil {
		t.Fatal("external CPU telemetry scheduled a host command")
	}
	if model.jobCPUTopLoading {
		t.Fatal("external CPU telemetry left loading state set")
	}
	updated, _ := model.switchToCPUTab()
	if updated.detailTab == DetailTabCPU {
		t.Fatal("external job entered CPU tab")
	}
	if header := model.renderTabHeader(); strings.Contains(header, "CPU") {
		t.Fatalf("external tab header exposes CPU: %q", header)
	}
}

func TestDashboardExternalDetailsShowObservationStaleness(t *testing.T) {
	observed := time.Now().Add(-11 * time.Minute).Unix()
	job := &db.Job{ID: 42, Backend: db.BackendSkyPilot, Status: db.StatusRunning}
	model := Model{externalBindings: map[int64]*db.ExternalJobBinding{
		job.ID: {
			JobID: job.ID, Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "99", ExternalTaskID: "task-a",
			ExternalClusterName: "cluster-a", RawStatus: "RUNNING", LastObservedAt: &observed,
			SyncWarning: "SkyPilot query timed out",
		},
	}}
	rendered := model.jobDetailContent(job)
	for _, want := range []string{"SkyPilot job 99 task task-a", "cluster-a", "RUNNING", "stale", "SkyPilot query timed out"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("external detail missing %q: %q", want, rendered)
		}
	}
}

func TestDashboardExternalDetailsShowUnconfirmedSubmissionRecovery(t *testing.T) {
	job := &db.Job{
		ID: 42, Backend: db.BackendSkyPilot, Status: db.StatusQueued,
		CreatedAt: time.Now().Add(-db.ExternalSubmissionUnconfirmedAfter).Unix(),
	}
	rendered := (Model{externalBindings: map[int64]*db.ExternalJobBinding{}}).jobDetailContent(job)
	for _, want := range []string{"submission unconfirmed", "running or billing", "weft sky import --job wj42 <sky-job-id>"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("old unbound dashboard detail missing %q: %q", want, rendered)
		}
	}

	// A minute inside the 5-minute window, not a second: the render calls
	// time.Now() again, and on a loaded machine a one-second margin can elapse
	// between the two calls, aging the job across the threshold.
	job.CreatedAt = time.Now().Add(-db.ExternalSubmissionUnconfirmedAfter + time.Minute).Unix()
	rendered = (Model{externalBindings: map[int64]*db.ExternalJobBinding{}}).jobDetailContent(job)
	if strings.Contains(rendered, "submission unconfirmed") || !strings.Contains(rendered, "binding unavailable") {
		t.Fatalf("young unbound dashboard detail = %q, want pending binding language", rendered)
	}
}
