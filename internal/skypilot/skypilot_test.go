package skypilot

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

type scriptedRunner struct {
	results []scriptedRunResult
	calls   int
	args    [][]string
}

type scriptedRunResult struct {
	out []byte
	err error
}

var errStreamingWriterRejected = errors.New("streaming writer rejected output")

type rejectingStreamingWriter struct{}

func (rejectingStreamingWriter) Write([]byte) (int, error) {
	return 0, errStreamingWriterRejected
}

func (r *scriptedRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.args = append(r.args, append([]string(nil), args...))
	if r.calls >= len(r.results) {
		return nil, fmt.Errorf("unexpected SkyPilot CLI call %d", r.calls+1)
	}
	result := r.results[r.calls]
	r.calls++
	return result.out, result.err
}

func TestListJobsRequestsAllManagedJobs(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{{out: []byte(`[]`)}}}
	if _, err := (Client{Runner: runner}).ListJobs(context.Background()); err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	got := strings.Join(runner.args[0], " ")
	if got != "jobs queue --all --verbose --output json" {
		t.Fatalf("ListJobs args = %q, want all managed jobs", got)
	}
}

func TestStreamLogsUsesCompleteIdentityAndExplicitFollowPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		follow bool
		want   string
	}{
		{name: "snapshot", follow: false, want: "jobs logs 42 task-a --no-follow --tail 50"},
		{name: "follow", follow: true, want: "jobs logs 42 task-a --follow --tail 50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{results: []scriptedRunResult{{out: []byte("logs")}}}
			if err := (Client{Runner: runner}).StreamLogs(context.Background(), "42", "task-a", tc.follow, 50, io.Discard, io.Discard); err != nil {
				t.Fatalf("StreamLogs: %v", err)
			}
			if got := strings.Join(runner.args[0], " "); got != tc.want {
				t.Fatalf("Logs args = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStreamLogsRequestsFullSnapshotExplicitly(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{{out: []byte("logs")}}}
	if err := (Client{Runner: runner}).StreamLogs(context.Background(), "42", "", false, 0, io.Discard, io.Discard); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if got := strings.Join(runner.args[0], " "); got != "jobs logs 42 --no-follow --tail 0" {
		t.Fatalf("Logs args = %q, want explicit full snapshot", got)
	}
}

func TestFollowLogsStreamsBeforeProcessExit(t *testing.T) {
	binDir := t.TempDir()
	skyPath := filepath.Join(binDir, "sky")
	if err := os.WriteFile(skyPath, []byte("#!/bin/sh\nprintf 'first line\\n'\nprintf 'diagnostic\\n' >&2\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write fake sky: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	var stderr strings.Builder
	go func() {
		defer writer.Close()
		done <- (Client{}).FollowLogs(ctx, "42", "task-a", 50, writer, &stderr)
	}()
	line := make(chan string, 1)
	go func() {
		got, _ := bufio.NewReader(reader).ReadString('\n')
		line <- got
	}()
	select {
	case got := <-line:
		if got != "first line\n" {
			t.Fatalf("first streamed line = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow output was buffered until process exit")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled follow returned nil error")
		}
		if got := stderr.String(); got != "diagnostic\n" {
			t.Fatalf("streamed stderr = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled follow process did not exit")
	}
}

func TestSnapshotLogsStreamStdoutSeparatelyBeforeProcessExit(t *testing.T) {
	binDir := t.TempDir()
	skyPath := filepath.Join(binDir, "sky")
	if err := os.WriteFile(skyPath, []byte("#!/bin/sh\nprintf 'retained line\\n'\nprintf 'diagnostic only\\n' >&2\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write fake sky: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	var stderr strings.Builder
	go func() {
		defer writer.Close()
		done <- (Client{}).StreamLogs(ctx, "42", "task-a", false, 0, writer, &stderr)
	}()
	line := make(chan string, 1)
	go func() {
		got, _ := bufio.NewReader(reader).ReadString('\n')
		line <- got
	}()
	select {
	case got := <-line:
		if got != "retained line\n" {
			t.Fatalf("first streamed snapshot line = %q", got)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("snapshot stdout was buffered until process exit")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled snapshot stream returned nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot stream did not return promptly after cancellation")
	}
	if !strings.Contains(stderr.String(), "diagnostic only") {
		t.Fatalf("snapshot stderr = %q", stderr.String())
	}
}

func TestFollowLogsWriterErrorTerminatesProcess(t *testing.T) {
	binDir := t.TempDir()
	skyPath := filepath.Join(binDir, "sky")
	script := "#!/bin/sh\ni=0\nwhile [ $i -lt 200 ]; do printf '0123456789'; i=$((i + 1)); done\nexec sleep 30\n"
	if err := os.WriteFile(skyPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sky: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	err := (Client{}).FollowLogs(ctx, "42", "task-a", 50, rejectingStreamingWriter{}, io.Discard)
	if !errors.Is(err, errStreamingWriterRejected) {
		t.Fatalf("FollowLogs error = %v, want writer rejection", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("writer rejection took %s; child process was not terminated promptly", elapsed)
	}
}

func TestStreamLogsRejectsNegativeTailBeforeRunnerCall(t *testing.T) {
	runner := &scriptedRunner{}
	if err := (Client{Runner: runner}).StreamLogs(context.Background(), "42", "", false, -1, io.Discard, io.Discard); err == nil {
		t.Fatal("negative log tail unexpectedly succeeded")
	}
	if len(runner.args) != 0 {
		t.Fatalf("negative log tail called runner with %v", runner.args)
	}
}

func TestParseJobsQueueJSONFlexibleShapes(t *testing.T) {
	jobs, err := ParseJobsQueueJSON([]byte(`{
		"jobs": [
			{
				"job_id": 42,
				"task_id": "train",
				"name": "weft-wj7",
				"status": "RUNNING",
				"cluster": "sky-cluster",
				"run": "python train.py"
			}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseJobsQueueJSON: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	job := jobs[0]
	if job.ID != "42" || job.TaskID != "train" || job.Name != "weft-wj7" || job.ClusterName != "sky-cluster" {
		t.Fatalf("parsed job = %+v", job)
	}
	if job.Command != "python train.py" {
		t.Fatalf("Command = %q", job.Command)
	}
}

func TestParseJobsQueueJSONCurrentVerboseManagedJobRecord(t *testing.T) {
	jobs, err := ParseJobsQueueJSON([]byte(`[{
		"job_id":42,
		"task_id":0,
		"job_name":"pipeline",
		"task_name":"train",
		"status":"FAILED_SETUP",
		"current_cluster_name":"sky-cluster-42",
		"details":"setup failed",
		"failure_reason":"CUDA unavailable",
		"submitted_at":1700000000.75,
		"start_at":1700000100.25,
		"end_at":1700000200.99,
		"metadata":{"owner":"alice"}
	}]`))
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ParseJobsQueueJSON = %+v, %v", jobs, err)
	}
	job := jobs[0]
	if job.ID != "42" || job.TaskID != "0" || job.TaskName != "train" || job.Name != "pipeline" {
		t.Fatalf("identity fields = %+v", job)
	}
	if job.ClusterName != "sky-cluster-42" || job.Command != "" || job.Message != "CUDA unavailable" || job.Dashboard != "" {
		t.Fatalf("verbose fields = %+v", job)
	}
	if job.SubmittedAt == nil || *job.SubmittedAt != 1700000000 || job.StartedAt == nil || *job.StartedAt != 1700000100 || job.EndedAt == nil || *job.EndedAt != 1700000200 {
		t.Fatalf("timestamp fields = %+v", job)
	}
	obs := ObservationFromJob(job, "project", "/work")
	if obs.SubmittedAt == nil || *obs.SubmittedAt != 1700000000 || obs.StartedAt == nil || *obs.StartedAt != 1700000100 || obs.EndedAt == nil || *obs.EndedAt != 1700000200 {
		t.Fatalf("observation timestamps = %+v", obs)
	}
	resolved, err := ResolveJobWithTask(jobs, "42", "train")
	if err != nil || resolved == nil || resolved.TaskID != "0" {
		t.Fatalf("ResolveJobWithTask(task name) = %+v, %v", resolved, err)
	}
}

func TestNormalizeStatus(t *testing.T) {
	tests := map[string]string{
		"submitted":    db.StatusQueued,
		"PROVISIONING": db.StatusQueued,
		"running":      db.StatusRunning,
		"WINDING_DOWN": db.StatusRunning,
		"succeeded":    db.StatusCompleted,
		"failed":       db.StatusFailed,
		"cancelling":   db.StatusRunning,
		"cancelled":    db.StatusCanceled,
		"stopped":      db.StatusKilled,
	}
	for raw, want := range tests {
		if got := NormalizeStatus(raw); got != want {
			t.Fatalf("NormalizeStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestBuildTaskYAML(t *testing.T) {
	task, err := BuildTaskYAML(SubmitOptions{
		Name:     "weft-wj12",
		Command:  "python train.py",
		WorkDir:  "/tmp/project",
		GPUClass: "a100",
		GPUCount: 2,
		EnvVars:  []string{"WANDB_MODE=offline"},
	})
	if err != nil {
		t.Fatalf("BuildTaskYAML: %v", err)
	}
	for _, want := range []string{
		`name: "weft-wj12"`,
		`workdir: "/tmp/project"`,
		`accelerators: A100:2`,
		`WANDB_MODE: "offline"`,
		`run: "python train.py"`,
	} {
		if !strings.Contains(task, want) {
			t.Fatalf("task YAML missing %q:\n%s", want, task)
		}
	}
}

func TestBuildTaskYAMLRejectsUnsupportedGPUMemory(t *testing.T) {
	mem := 80
	_, err := BuildTaskYAML(SubmitOptions{
		Command: "python train.py", GPUMemGB: &mem,
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("BuildTaskYAML error = %v, want unsupported GPU memory", err)
	}
}

func TestSubmitClassifiesTimeoutAsUnconfirmed(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{{err: context.DeadlineExceeded}}}
	_, _, err := (Client{Runner: runner}).Submit(context.Background(), SubmitOptions{
		Name:    "weft-wj12",
		Command: "python train.py",
	})
	if err == nil {
		t.Fatal("Submit returned nil error, want unconfirmed submission")
	}
	var unconfirmed *SubmissionUnconfirmedError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("Submit error = %T %v, want SubmissionUnconfirmedError", err, err)
	}
}

func TestSubmitClassifiesUnresolvedIdentityAsUnconfirmed(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{
		{out: []byte("Launch accepted\n")},
		{err: fmt.Errorf("queue query failed")},
	}}
	_, _, err := (Client{Runner: runner}).Submit(context.Background(), SubmitOptions{
		Name:    "weft-wj12",
		Command: "python train.py",
	})
	if err == nil {
		t.Fatal("Submit returned nil error, want unconfirmed submission")
	}
	var unconfirmed *SubmissionUnconfirmedError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("Submit error = %T %v, want SubmissionUnconfirmedError", err, err)
	}
}

func TestSubmitKeepsDefinitiveLaunchFailureDistinct(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{{err: &SubmissionRefusedError{Cause: fmt.Errorf("quota refused")}}}}
	_, _, err := (Client{Runner: runner}).Submit(context.Background(), SubmitOptions{
		Name:    "weft-wj12",
		Command: "python train.py",
	})
	if err == nil {
		t.Fatal("Submit returned nil error, want definitive failure")
	}
	var unconfirmed *SubmissionUnconfirmedError
	if errors.As(err, &unconfirmed) {
		t.Fatalf("definitive launch failure was classified as unconfirmed: %v", err)
	}
}

func TestSubmitClassifiesGenericLaunchErrorAsUnconfirmed(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{{err: fmt.Errorf("remote API transport failed")}}}
	_, _, err := (Client{Runner: runner}).Submit(context.Background(), SubmitOptions{
		Name:    "weft-wj12",
		Command: "python train.py",
	})
	if err == nil {
		t.Fatal("Submit returned nil error, want unconfirmed submission")
	}
	var unconfirmed *SubmissionUnconfirmedError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("Submit error = %T %v, want SubmissionUnconfirmedError", err, err)
	}
}

func TestSubmitFallbackIgnoresUnlabelledNumbers(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{
		{out: []byte("Launching 1 task; request accepted\n")},
		{err: fmt.Errorf("queue query failed")},
	}}
	job, _, err := (Client{Runner: runner}).Submit(context.Background(), SubmitOptions{
		Name:    "weft-wj12",
		Command: "python train.py",
	})
	if err == nil || job != nil {
		t.Fatalf("Submit = job %+v, err %v; want unconfirmed identity", job, err)
	}
	var unconfirmed *SubmissionUnconfirmedError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("Submit error = %T %v, want SubmissionUnconfirmedError", err, err)
	}
}

func TestSubmitFallbackAcceptsLabelledJobID(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{
		{out: []byte("Launching 1 task\nManaged job ID: 42 submitted successfully\n")},
		{err: fmt.Errorf("queue query failed")},
	}}
	job, _, err := (Client{Runner: runner}).Submit(context.Background(), SubmitOptions{
		Name:    "weft-wj12",
		Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job == nil || job.ID != "42" {
		t.Fatalf("Submit job = %+v, want ID 42", job)
	}
	wantPrefix := []string{"jobs", "launch", "-y", "-d", "--name", "weft-wj12"}
	got := runner.args[0]
	if len(got) != len(wantPrefix)+1 || strings.Join(got[:len(wantPrefix)], " ") != strings.Join(wantPrefix, " ") {
		t.Fatalf("Submit launch args = %q, want prefix %q plus task path", got, wantPrefix)
	}
}

func TestSubmitLabelledJobIDBeatsStaleSameNameQueueRow(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{
		{out: []byte("Managed Job ID: 43\n")},
		{out: []byte(`[
			{"job_id":41,"name":"43","status":"SUCCEEDED"},
			{"job_id":42,"name":"weft-wj7","status":"RUNNING"}
		]`)},
	}}
	job, _, err := (Client{Runner: runner}).Submit(context.Background(), SubmitOptions{
		Name:    "weft-wj7",
		Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job == nil || job.ID != "43" || job.Name != "weft-wj7" || job.Status != "submitted" {
		t.Fatalf("Submit job = %+v, want provisional fresh ID 43", job)
	}
}

func TestResolveJobPrefersExactIDOverAnotherRowsName(t *testing.T) {
	jobs := []Job{
		{ID: "99", Name: "42", Status: "SUCCEEDED"},
		{ID: "42", Name: "actual", Status: "RUNNING"},
	}
	job, err := ResolveJob(jobs, "42")
	if err != nil {
		t.Fatalf("ResolveJob: %v", err)
	}
	if job == nil || job.ID != "42" {
		t.Fatalf("ResolveJob = %+v, want exact job ID 42", job)
	}
}

func TestResolveJobRejectsAmbiguousExactID(t *testing.T) {
	jobs := []Job{
		{ID: "42", TaskID: "task-a", Status: "RUNNING"},
		{ID: "42", TaskID: "task-b", Status: "SUCCEEDED"},
	}
	if job, err := ResolveJob(jobs, "42"); err == nil || job != nil {
		t.Fatalf("ResolveJob = %+v, %v; want ambiguity error", job, err)
	}
}

func TestResolveJobNeverTreatsTaskIdentityAsGloballyUnique(t *testing.T) {
	jobs := []Job{
		{ID: "42", TaskID: "0", TaskName: "train", Name: "pipeline-a"},
		{ID: "99", TaskID: "0", TaskName: "train", Name: "pipeline-b"},
	}
	for _, task := range []string{"0", "train"} {
		job, err := ResolveJob(jobs, task)
		if err != nil || job != nil {
			t.Fatalf("ResolveJob(%q) = %+v, %v; want no global task match", task, job, err)
		}
	}
}

func TestResolveJobWithTaskUsesCompleteIdentity(t *testing.T) {
	jobs := []Job{
		{ID: "42", TaskID: "0", TaskName: "train", Name: "pipeline-a", Status: "RUNNING"},
		{ID: "42", TaskID: "1", TaskName: "eval", Name: "pipeline-a", Status: "RUNNING"},
		{ID: "99", TaskID: "0", TaskName: "train", Name: "pipeline-b", Status: "RUNNING"},
	}
	for _, task := range []string{"0", "train"} {
		job, err := ResolveJobWithTask(jobs, "42", task)
		if err != nil {
			t.Fatalf("ResolveJobWithTask(42, %q): %v", task, err)
		}
		if job == nil || job.ID != "42" || job.TaskID != "0" {
			t.Fatalf("ResolveJobWithTask(42, %q) = %+v, want 42/0", task, job)
		}
	}
	if job, err := ResolveJobWithTask(jobs, "42", "missing"); err != nil || job != nil {
		t.Fatalf("ResolveJobWithTask missing = %+v, %v; want nil, nil", job, err)
	}
}

func TestResolveJobByNameRejectsAmbiguity(t *testing.T) {
	jobs := []Job{
		{ID: "42", Name: "weft-wj7", Status: "RUNNING"},
		{ID: "43", Name: "weft-wj7", Status: "SUCCEEDED"},
	}
	if job, err := ResolveJobByName(jobs, "weft-wj7"); err == nil || job != nil {
		t.Fatalf("ResolveJobByName = %+v, %v; want ambiguity error", job, err)
	}
}

func TestSubmitTreatsAmbiguousGeneratedNameAsUnconfirmed(t *testing.T) {
	runner := &scriptedRunner{results: []scriptedRunResult{
		{out: []byte("Launch accepted\n")},
		{out: []byte(`[
			{"job_id":42,"name":"weft-wj7","status":"RUNNING"},
			{"job_id":43,"name":"weft-wj7","status":"RUNNING"}
		]`)},
	}}
	job, _, err := (Client{Runner: runner}).Submit(context.Background(), SubmitOptions{
		Name:    "weft-wj7",
		Command: "python train.py",
	})
	if err == nil || job != nil {
		t.Fatalf("Submit = %+v, %v; want unconfirmed ambiguous identity", job, err)
	}
	var unconfirmed *SubmissionUnconfirmedError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("Submit error = %T %v, want SubmissionUnconfirmedError", err, err)
	}
}
