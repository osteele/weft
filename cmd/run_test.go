package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
	"github.com/spf13/cobra"
)

func TestRecordQueuedJobSingleWriterUsesDaemonWhenAvailable(t *testing.T) {
	database := db.SetupTestDB(t)

	orig := recordQueuedJobMutationFunc
	t.Cleanup(func() { recordQueuedJobMutationFunc = orig })
	recordQueuedJobMutationFunc = func(_ context.Context, _ *sql.DB, params ops.QueueJobParams) (int64, error) {
		if params.Command != "echo daemon" {
			t.Fatalf("daemon params command = %q", params.Command)
		}
		return 4242, nil
	}

	jobID, err := recordQueuedJobSingleWriter(database, ops.QueueJobParams{
		WorkingDir: "/tmp/project",
		Command:    "echo daemon",
	})
	if err != nil {
		t.Fatalf("recordQueuedJobSingleWriter: %v", err)
	}
	if jobID != 4242 {
		t.Fatalf("jobID = %d, want daemon result", jobID)
	}
}

func TestRecordQueuedJobSingleWriterFallsBackWithoutDaemon(t *testing.T) {
	database := db.SetupTestDB(t)

	orig := recordQueuedJobMutationFunc
	t.Cleanup(func() { recordQueuedJobMutationFunc = orig })
	recordQueuedJobMutationFunc = func(_ context.Context, database *sql.DB, params ops.QueueJobParams) (int64, error) {
		return ops.RecordQueuedJob(database, params)
	}

	jobID, err := recordQueuedJobSingleWriter(database, ops.QueueJobParams{
		WorkingDir:  "/tmp/project",
		Command:     "echo fallback",
		Description: "fallback",
	})
	if err != nil {
		t.Fatalf("recordQueuedJobSingleWriter: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.Command != "echo fallback" {
		t.Fatalf("job = %+v", job)
	}
}

func TestParseCdPrefix(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		wantDir  string
		wantRest string
	}{
		{"unquoted with &&", "cd /path/to/dir && python train.py", "/path/to/dir", "python train.py"},
		{"unquoted with semicolon", "cd /path/to/dir; python train.py", "/path/to/dir", "python train.py"},
		{"single-quoted path", "cd '/path/to my dir' && python train.py", "/path/to my dir", "python train.py"},
		{"double-quoted path", `cd "/path/to my dir" && python train.py`, "/path/to my dir", "python train.py"},
		{"no cd prefix", "python train.py", "", "python train.py"},
		{"cd only no separator", "cd /path/to/dir", "", "cd /path/to/dir"},
		{"tilde path", "cd ~/projects && make", "~/projects", "make"},
		{"leading whitespace", "  cd /foo && bar", "/foo", "bar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, rest := parseCdPrefix(tt.command)
			if dir != tt.wantDir || rest != tt.wantRest {
				t.Errorf("parseCdPrefix(%q) = (%q, %q), want (%q, %q)",
					tt.command, dir, rest, tt.wantDir, tt.wantRest)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"safe string", "hello", "hello"},
		{"empty string", "", "''"},
		{"spaces", "hello world", "'hello world'"},
		{"special chars", "foo$bar", "'foo$bar'"},
		{"inner single quotes", "it's", "'it'\"'\"'s'"},
		{"tilde", "~/path", "'~/path'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFastSubmitPendingReasonForRecentOnPrem(t *testing.T) {
	const current = "placement pending"
	unplaced := &placement.PlacementPlan{Unplaced: true}

	if got := fastSubmitPendingReasonForRecentOnPrem(current, []string{"EXP-127"}, true, unplaced); got != current {
		t.Fatalf("cloud-eligible reason = %q, want %q", got, current)
	}
	if got := fastSubmitPendingReasonForRecentOnPrem(current, []string{db.TagInventory}, true, unplaced); got != "waiting for an eligible on-prem host" {
		t.Fatalf("inventory unplaced reason = %q", got)
	}
	if got := fastSubmitPendingReasonForRecentOnPrem(current, []string{db.TagInventory}, false, nil); got != "recent host state unavailable" {
		t.Fatalf("inventory stale reason = %q", got)
	}
}

func TestShouldValidateRentalJobImage(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		tags   []string
		draft  bool
		dryRun bool
		want   bool
	}{
		{name: "auto rental", want: true},
		{name: "launch host", host: db.LaunchHost(42), want: true},
		{name: "inventory tag", tags: []string{db.TagInventory}, want: false},
		{name: "on-prem host", host: "cool30", want: false},
		{name: "draft", draft: true, want: false},
		{name: "dry run", dryRun: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldValidateRentalJobImage(tt.host, tt.tags, tt.draft, tt.dryRun); got != tt.want {
				t.Fatalf("shouldValidateRentalJobImage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunRunValidatesRentalImageBeforeRecordingJob(t *testing.T) {
	database := db.SetupTestDB(t)
	database.Close()

	dir := t.TempDir()
	resetRunGlobals(t)
	t.Cleanup(func() {
		validateRentalJobImageFunc = campaign.ValidateJobImageAvailability
	})
	runDir = dir
	runDescription = "image probe failure"
	runGPU = "nvidia>=24GB"

	sentinel := errors.New("image denied")
	called := false
	validateRentalJobImageFunc = func(ctx context.Context, cfg *config.Config, localDir, command string) error {
		called = true
		if localDir != dir {
			t.Fatalf("localDir = %q, want %q", localDir, dir)
		}
		if command != "python train.py" {
			t.Fatalf("command = %q, want python train.py", command)
		}
		return sentinel
	}

	cmd := newRunTestCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := runRun(cmd, []string{"python train.py"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("runRun error = %v, want sentinel\noutput:\n%s", err, out.String())
	}
	if !called {
		t.Fatal("validateRentalJobImageFunc was not called")
	}

	readDB, err := db.Open()
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer readDB.Close()
	jobs, err := db.ListJobsWithMaxAge(readDB, "", "", 10, 0, nil, "")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs len = %d, want 0 when image validation fails", len(jobs))
	}
}

func TestPathHasHomePrefix(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	tests := []struct {
		name string
		dir  string
		home string
		want bool
	}{
		{"exact match", home, home, true},
		{"subdirectory", filepath.Join(home, "projects"), home, true},
		{"sibling", home + "-other", home, false},
		{"unrelated", "/tmp/foo", home, false},
		{"trailing slash cleaned", home + "/", home, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pathHasHomePrefix(tt.dir, tt.home)
			if got != tt.want {
				t.Errorf("pathHasHomePrefix(%q, %q) = %v, want %v", tt.dir, tt.home, got, tt.want)
			}
		})
	}
}

func TestRejectUnlockedTorchCloudRuntimeRejectsPyprojectRange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(`
[project]
dependencies = ["torch>=2.5"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := rejectUnlockedTorchCloudRuntime(dir, "", "", "", nil, nil)
	if err == nil {
		t.Fatal("expected unlocked torch range to be rejected")
	}
	if got := err.Error(); !strings.Contains(got, "torch >=2.5") || !strings.Contains(got, "uv.lock is missing") {
		t.Fatalf("error = %q, want torch range and missing lockfile", got)
	}
}

func TestRejectUnlockedTorchCloudRuntimeAllowsPinnedHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(`
[project]
dependencies = ["torch>=2.5"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rejectUnlockedTorchCloudRuntime(dir, "", "cool30", "", nil, nil); err != nil {
		t.Fatalf("pinned host rejected: %v", err)
	}
}

func TestRejectUnlockedTorchCloudRuntimeRequiresCUDAFloorForExactPyprojectPin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(`
[project]
dependencies = ["torch==2.6.0"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := rejectUnlockedTorchCloudRuntime(dir, "", "", "", nil, nil)
	if err == nil {
		t.Fatal("expected exact pyproject pin without CUDA variant to be rejected")
	}
	if !strings.Contains(err.Error(), "does not expose the CUDA wheel variant") {
		t.Fatalf("error = %q, want CUDA wheel variant diagnosis", err.Error())
	}
	if err := rejectUnlockedTorchCloudRuntime(dir, "", "", "12.4", nil, nil); err != nil {
		t.Fatalf("explicit CUDA floor rejected: %v", err)
	}
}

func TestRejectUnlockedTorchCloudRuntimeAllowsScriptTorchRange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(`
[project]
dependencies = ["torch==2.6.0"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte(`# /// script
# dependencies = ["torch>=2.2", "transformers>=4.44"]
# ///
import torch
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rejectUnlockedTorchCloudRuntime(dir, "uv run train.py", "", "", nil, nil); err != nil {
		t.Fatalf("script torch range should be handled by inferred floor: %v", err)
	}
}

func TestSubmitJobToCloudReuse_AckReceived(t *testing.T) {
	prev := submitJobsToInstanceFunc
	t.Cleanup(func() { submitJobsToInstanceFunc = prev })

	submitJobsToInstanceFunc = func(context.Context, *sql.DB, *r2.Client, int64, []*db.Job) error {
		return nil
	}

	outcome, _, err := submitJobToCloudReuse(nil, nil, 42, &db.Job{ID: 1})
	if err != nil {
		t.Fatalf("submitJobToCloudReuse: %v", err)
	}
	if outcome != cloudReuseAckReceived {
		t.Fatalf("outcome = %v, want %v", outcome, cloudReuseAckReceived)
	}
}

func TestSubmitJobToCloudReuse_AckNotObserved(t *testing.T) {
	prev := submitJobsToInstanceFunc
	t.Cleanup(func() { submitJobsToInstanceFunc = prev })

	submitJobsToInstanceFunc = func(context.Context, *sql.DB, *r2.Client, int64, []*db.Job) error {
		return context.DeadlineExceeded
	}

	outcome, _, err := submitJobToCloudReuse(nil, nil, 42, &db.Job{ID: 1})
	if err != nil {
		t.Fatalf("submitJobToCloudReuse: %v", err)
	}
	if outcome != cloudReuseAckNotObserved {
		t.Fatalf("outcome = %v, want %v", outcome, cloudReuseAckNotObserved)
	}
}

func TestSubmitJobToCloudReuse_SubmitFailure(t *testing.T) {
	prev := submitJobsToInstanceFunc
	t.Cleanup(func() { submitJobsToInstanceFunc = prev })

	wantErr := errors.New("r2 unavailable")
	submitJobsToInstanceFunc = func(context.Context, *sql.DB, *r2.Client, int64, []*db.Job) error {
		return wantErr
	}

	outcome, _, err := submitJobToCloudReuse(nil, nil, 42, &db.Job{ID: 1})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if outcome != cloudReuseSubmitFailed {
		t.Fatalf("outcome = %v, want %v", outcome, cloudReuseSubmitFailed)
	}
}

func TestSameRetryCommandShape(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{
			name: "exact command",
			a:    "uv run python train.py --lr 1e-4",
			b:    "uv run python train.py --lr 1e-4",
			want: true,
		},
		{
			name: "same script different args",
			a:    "uv run python train.py --lr 1e-4",
			b:    "uv run python train.py --lr 5e-5",
			want: true,
		},
		{
			name: "same script different runner",
			a:    "python train.py",
			b:    "uv run python train.py",
			want: true,
		},
		{
			name: "different script",
			a:    "python train.py",
			b:    "python eval.py",
			want: false,
		},
		{
			name: "ambiguous multiple scripts",
			a:    "python train.py && python eval.py",
			b:    "python train.py",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameRetryCommandShape(tt.a, tt.b); got != tt.want {
				t.Fatalf("sameRetryCommandShape(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestFindGraceRetryCandidate_MatchesFailedSameScript(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now()
	deadline := now.Add(5 * time.Minute).Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:        db.LaunchStatusGrace,
		Provider:      "vastai",
		GPUClass:      "a100",
		GPUMemGB:      80,
		GraceDeadline: &deadline,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	priorID, err := db.RecordQueued(database, "", "/tmp/project", "uv run python serve.py --old", "failed")
	if err != nil {
		t.Fatalf("RecordQueued prior: %v", err)
	}
	if err := db.SetJobLaunchID(database, priorID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	ended := now.Add(-time.Minute).Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ?, exit_code = 1, cloud_outcome = ? WHERE job_id = ? AND launch_id = ?`,
		db.StatusFailed, ended, db.AttemptOutcomeFailed, priorID, instanceID,
	); err != nil {
		t.Fatalf("mark prior failed: %v", err)
	}

	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python serve.py --fixed", "retry")
	if err != nil {
		t.Fatalf("RecordQueued retry: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	inst, prior, err := findGraceRetryCandidate(database, job, now)
	if err != nil {
		t.Fatalf("findGraceRetryCandidate: %v", err)
	}
	if inst == nil || inst.ID != instanceID {
		t.Fatalf("instance = %#v, want id %d", inst, instanceID)
	}
	if prior == nil || prior.ID != priorID {
		t.Fatalf("prior = %#v, want id %d", prior, priorID)
	}
}

func TestFindGraceRetryCandidate_IgnoresExpiredGrace(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now()
	deadline := now.Add(-time.Minute).Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:        db.LaunchStatusGrace,
		Provider:      "vastai",
		GPUClass:      "a100",
		GPUMemGB:      80,
		GraceDeadline: &deadline,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	priorID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "failed")
	if err != nil {
		t.Fatalf("RecordQueued prior: %v", err)
	}
	if err := db.SetJobLaunchID(database, priorID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ?, exit_code = 1, cloud_outcome = ? WHERE job_id = ? AND launch_id = ?`,
		db.StatusFailed, now.Add(-2*time.Minute).Unix(), db.AttemptOutcomeFailed, priorID, instanceID,
	); err != nil {
		t.Fatalf("mark prior failed: %v", err)
	}

	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "retry")
	if err != nil {
		t.Fatalf("RecordQueued retry: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	inst, prior, err := findGraceRetryCandidate(database, job, now)
	if err != nil {
		t.Fatalf("findGraceRetryCandidate: %v", err)
	}
	if inst != nil || prior != nil {
		t.Fatalf("candidate = (%#v, %#v), want nil", inst, prior)
	}
}

func TestScanRunScriptMetaRejectsMalformedPEP723(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte(`# /// script
# [tool.weft]
# note = "contains / and never closes
# inputs = ["hf:gpt2"]
# ///
print("hello")
`), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	_, err := scanRunScriptMeta(dir, "uv run python train.py")
	if err == nil {
		t.Fatal("expected malformed metadata to fail")
	}
	if !strings.Contains(err.Error(), "invalid script metadata") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHasEnvAssignment(t *testing.T) {
	if !hasEnvAssignment([]string{"HF_HUB_OFFLINE=0"}, "HF_HUB_OFFLINE") {
		t.Fatal("expected exact env assignment to match")
	}
	if hasEnvAssignment([]string{"MY_HF_HUB_OFFLINE=0"}, "HF_HUB_OFFLINE") {
		t.Fatal("expected prefixed env name not to match")
	}
}

func TestCommandRecommendationsSuppressVLLMWhenPEP723RunsViaUVScriptShebang(t *testing.T) {
	dir := t.TempDir()
	scriptDir := filepath.Join(dir, "scripts")
	if err := os.Mkdir(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	script := filepath.Join(scriptDir, "profile_inference_vllm.py")
	content := `#!/usr/bin/env -S uv run --script
# /// script
# dependencies = ["vllm==0.19.1", "pynvml>=12.0"]
# ///
import vllm
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	got := commandRecommendations("scripts/profile_inference_vllm.py --models mistralai/Mixtral-8x7B-v0.1", dir)
	if len(got) != 0 {
		t.Fatalf("recommendations = %v, want none", got)
	}
}

func TestCommandRecommendationsWarnVLLMWhenPEP723RunsThroughPython(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "profile_inference_vllm.py")
	content := `#!/usr/bin/env python3
# /// script
# dependencies = ["vllm==0.19.1"]
# ///
import vllm
`
	if err := os.WriteFile(script, []byte(content), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	got := commandRecommendations("uv run python profile_inference_vllm.py", dir)
	if len(got) != 1 || !strings.Contains(got[0], "vLLM jobs should declare vllm") {
		t.Fatalf("recommendations = %v, want vLLM runtime tip", got)
	}
}

func TestCommandRecommendationsSuppressVLLMWhenPyprojectRunsViaUV(t *testing.T) {
	dir := t.TempDir()
	pyproject := `[project]
name = "example"
version = "0.0.0"
dependencies = ["vllm==0.19.1"]
`
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(pyproject), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}

	got := commandRecommendations("uv run python scripts/profile_inference_vllm.py", dir)
	if len(got) != 0 {
		t.Fatalf("recommendations = %v, want none", got)
	}
}

func TestCommandRecommendationsSuppressSGLangWhenPEP723RunsViaUVScriptShebang(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "serve_sglang.py")
	content := `#!/usr/bin/env -S uv run --script
# /// script
# dependencies = ["sglang==0.5.10.post1"]
# ///
import sglang
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	got := commandRecommendations("serve_sglang.py --model Qwen/Qwen2-7B", dir)
	if len(got) != 0 {
		t.Fatalf("recommendations = %v, want none", got)
	}
}

func TestPersistDraftArtifactFields(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordDraftJobWithGPU(database, "cool30", "/tmp/project", "echo hello", "draft", "")
	if err != nil {
		t.Fatalf("RecordDraftJobWithGPU: %v", err)
	}

	inputs := []string{"hf-dataset:allenai/c4"}
	outputs := []string{"output/representations/model_a_train.pkl"}
	outputDirs := []string{"output/representations"}
	produces := []string{"output/representations/model_a_train.pkl"}
	needs := []string{
		"output/representations/model_b_train.pkl:1285",
		"output/representations/model_b_dev.pkl:1285",
	}

	if err := persistDraftArtifactFields(database, jobID, inputs, outputs, outputDirs, produces, needs); err != nil {
		t.Fatalf("persistDraftArtifactFields: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if !reflect.DeepEqual(job.Inputs, inputs) {
		t.Fatalf("Inputs = %v, want %v", job.Inputs, inputs)
	}
	if !reflect.DeepEqual(job.Outputs, outputs) {
		t.Fatalf("Outputs = %v, want %v", job.Outputs, outputs)
	}
	if !reflect.DeepEqual(job.OutputDirs, outputDirs) {
		t.Fatalf("OutputDirs = %v, want %v", job.OutputDirs, outputDirs)
	}
	if !reflect.DeepEqual(job.Produces, produces) {
		t.Fatalf("Produces = %v, want %v", job.Produces, produces)
	}
	if !reflect.DeepEqual(job.Needs, needs) {
		t.Fatalf("Needs = %v, want %v", job.Needs, needs)
	}
}

func TestHFOfflineEnvVarsModelOnly(t *testing.T) {
	got := hfOfflineEnvVars([]string{"hf:gpt2"})
	if !reflect.DeepEqual(got, []string{"TRANSFORMERS_OFFLINE=1"}) {
		t.Fatalf("hfOfflineEnvVars = %v, want transformers-only offline", got)
	}
}

func TestHFOfflineEnvVarsDatasetOnlyDoesNotForceDatasetsOffline(t *testing.T) {
	got := hfOfflineEnvVars([]string{"hf-dataset:wikitext"})
	if len(got) != 0 {
		t.Fatalf("hfOfflineEnvVars = %v, want no blanket dataset offline env", got)
	}
}

func TestRunDraftWithoutHostRecordsDraft(t *testing.T) {
	database := db.SetupTestDB(t)
	database.Close()

	dir := t.TempDir()
	resetRunGlobals(t)
	runDraft = true
	runDir = dir
	runDescription = "draft without host"
	runGPU = "a100>=80GB"

	cmd := newRunTestCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := runRun(cmd, []string{"python train.py"}); err != nil {
		t.Fatalf("runRun: %v\noutput:\n%s", err, out.String())
	}

	readDB, err := db.Open()
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer readDB.Close()
	jobs, err := db.ListJobsWithMaxAge(readDB, "", "", 10, 0, nil, "")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs len = %d, want 1", len(jobs))
	}
	job := jobs[0]
	if job.Status != db.StatusDraft {
		t.Fatalf("status = %q, want %q", job.Status, db.StatusDraft)
	}
	if job.Host != "" {
		t.Fatalf("host = %q, want empty", job.Host)
	}
	if job.Project != filepath.Base(dir) {
		t.Fatalf("project = %q, want %q", job.Project, filepath.Base(dir))
	}
	if job.GPUClass != "a100" {
		t.Fatalf("gpu_class = %q, want a100", job.GPUClass)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 80 {
		t.Fatalf("gpu_mem_gb = %v, want 80", job.GPUMemGB)
	}
	if !strings.Contains(out.String(), "Draft job #") {
		t.Fatalf("output missing draft confirmation:\n%s", out.String())
	}
}

func TestRunDraftWithPositionalHostRecordsHost(t *testing.T) {
	database := db.SetupTestDB(t)
	database.Close()

	dir := t.TempDir()
	resetRunGlobals(t)
	runDraft = true
	runDir = dir
	runDescription = "draft positional host"
	runGPU = "nvidia>=24GB"

	cmd := newRunTestCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := runCmd.Args(cmd, []string{"cool30", "python train.py"}); err != nil {
		t.Fatalf("run args: %v", err)
	}
	if err := runRun(cmd, []string{"cool30", "python train.py"}); err != nil {
		t.Fatalf("runRun: %v\noutput:\n%s", err, out.String())
	}

	readDB, err := db.Open()
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer readDB.Close()
	jobs, err := db.ListJobsWithMaxAge(readDB, "", "", 10, 0, nil, "")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs len = %d, want 1", len(jobs))
	}
	job := jobs[0]
	if job.Host != "cool30" {
		t.Fatalf("host = %q, want cool30", job.Host)
	}
	if job.GPUClass != "nvidia" {
		t.Fatalf("gpu_class = %q, want nvidia", job.GPUClass)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 24 {
		t.Fatalf("gpu_mem_gb = %v, want 24", job.GPUMemGB)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.Host != "cool30" {
		t.Fatalf("cli host override = %#v, want cool30", job.CLIResourceOverrides)
	}
	if !strings.Contains(out.String(), "saved for cool30") {
		t.Fatalf("output missing host confirmation:\n%s", out.String())
	}
}

func TestRunForwardsDashPassthroughArgs(t *testing.T) {
	database := db.SetupTestDB(t)
	database.Close()

	dir := t.TempDir()
	resetRunGlobals(t)
	runDraft = true
	runDir = dir
	runDescription = "dash passthrough"

	cmd := newRunTestCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	// Drive the flagset so cobra records ArgsLenAtDash, mirroring how the real
	// command parses `weft run uv run x.py -- --models gpt2`.
	argv := []string{"uv", "run", "scripts/profile.py", "--", "--models", "gpt2", "--max-model-len", "4096"}
	if err := cmd.Flags().Parse(argv); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	args := cmd.Flags().Args()
	if got := cmd.ArgsLenAtDash(); got != 3 {
		t.Fatalf("ArgsLenAtDash = %d, want 3", got)
	}

	// Regression: the Args validator previously rejected >2 positionals, so the
	// dashed form printed help instead of submitting.
	if err := runCmd.Args(cmd, args); err != nil {
		t.Fatalf("run args rejected dashed form: %v", err)
	}
	if err := runRun(cmd, args); err != nil {
		t.Fatalf("runRun: %v\noutput:\n%s", err, out.String())
	}

	readDB, err := db.Open()
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer readDB.Close()
	jobs, err := db.ListJobsWithMaxAge(readDB, "", "", 10, 0, nil, "")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs len = %d, want 1", len(jobs))
	}
	job := jobs[0]
	want := "uv run scripts/profile.py --models gpt2 --max-model-len 4096"
	if job.Command != want {
		t.Fatalf("command = %q, want %q", job.Command, want)
	}
	// No positional host: the dashed form leaves placement to auto-select.
	if job.Host != "" {
		t.Fatalf("host = %q, want empty (auto-place)", job.Host)
	}
}

func TestEvaluateRecentOnPremPlacementRequiresFreshMetrics(t *testing.T) {
	database := db.SetupTestDB(t)
	inventory.UseTestHosts(t)

	plan, recent, err := evaluateRecentOnPremPlacement(database, placement.Constraints{GPUClass: "a100", GPUMemGB: 80})
	if err != nil {
		t.Fatalf("evaluateRecentOnPremPlacement: %v", err)
	}
	if recent || plan != nil {
		t.Fatalf("got recent=%v plan=%#v, want stale/no plan", recent, plan)
	}
}

func TestEvaluateRecentOnPremPlacementUsesDBMetricsOnly(t *testing.T) {
	database := db.SetupTestDB(t)
	inventory.UseTestHosts(t)
	now := time.Now().Unix()
	for _, host := range []string{"host-alpha", "host-beta", "host-gamma"} {
		_, err := database.Exec(`INSERT INTO host_contention_obs (host, gpu_pct, cpu_pct, queue_depth, gpu_jobs_queued, observed_at)
			VALUES (?, ?, ?, ?, ?, ?)`, host, 5, 10, 0, 0, now)
		if err != nil {
			t.Fatalf("insert contention obs: %v", err)
		}
	}

	plan, recent, err := evaluateRecentOnPremPlacement(database, placement.Constraints{GPUClass: "a100", GPUMemGB: 80})
	if err != nil {
		t.Fatalf("evaluateRecentOnPremPlacement: %v", err)
	}
	if !recent {
		t.Fatal("recent = false, want true")
	}
	if plan == nil || plan.Fast == nil || plan.Fast.OnPrem == nil {
		t.Fatalf("plan missing on-prem candidate: %#v", plan)
	}
	if got := plan.Fast.OnPrem.Host; got != "host-alpha" {
		t.Fatalf("selected host = %q, want host-alpha", got)
	}
}

func TestEvaluateRecentOnPremPlacementSkipsRentalTaggedJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	inventory.UseTestHosts(t)
	now := time.Now().Unix()
	for _, host := range []string{"host-alpha", "host-beta", "host-gamma"} {
		_, err := database.Exec(`INSERT INTO host_contention_obs (host, gpu_pct, cpu_pct, queue_depth, gpu_jobs_queued, observed_at)
			VALUES (?, ?, ?, ?, ?, ?)`, host, 5, 10, 0, 0, now)
		if err != nil {
			t.Fatalf("insert contention obs: %v", err)
		}
	}

	tests := []struct {
		name        string
		constraints placement.Constraints
	}{
		{
			name:        "rental tag",
			constraints: placement.Constraints{GPUClass: "a100", GPUMemGB: 80, Tags: []string{db.TagRental}},
		},
		{
			name:        "provider tag",
			constraints: placement.Constraints{GPUClass: "a100", GPUMemGB: 80, Tags: []string{db.TagProviderVastai}, Provider: "vastai"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, recent, err := evaluateRecentOnPremPlacement(database, tt.constraints)
			if err != nil {
				t.Fatalf("evaluateRecentOnPremPlacement: %v", err)
			}
			if !recent {
				t.Fatal("recent = false, want true")
			}
			if plan == nil || !plan.Unplaced {
				t.Fatalf("plan = %#v, want unplaced", plan)
			}
			if plan.Fast != nil || plan.Cheap != nil || plan.Fastest != nil {
				t.Fatalf("plan selected on-prem candidate despite rental/provider constraint: %#v", plan)
			}
		})
	}
}

func TestEvaluateRecentOnPremPlacementEnforcesGPUCount(t *testing.T) {
	// wb41 regression on the fast-submit path: the fast-submit scorer must
	// apply the same GPU-count eligibility guard as PlaceOnPrem. host-alpha
	// holds 2x A100, so a 3x A100 job must stay unplaced instead of being
	// dispatched and failing the runner preflight after prewarm spend.
	database := db.SetupTestDB(t)
	inventory.UseTestHosts(t)
	now := time.Now().Unix()
	for _, host := range []string{"host-alpha", "host-beta", "host-gamma"} {
		_, err := database.Exec(`INSERT INTO host_contention_obs (host, gpu_pct, cpu_pct, queue_depth, gpu_jobs_queued, observed_at)
			VALUES (?, ?, ?, ?, ?, ?)`, host, 5, 10, 0, 0, now)
		if err != nil {
			t.Fatalf("insert contention obs: %v", err)
		}
	}

	plan, recent, err := evaluateRecentOnPremPlacement(database, placement.Constraints{GPUClass: "a100", NumGPUs: 3})
	if err != nil {
		t.Fatalf("evaluateRecentOnPremPlacement: %v", err)
	}
	if !recent {
		t.Fatal("recent = false, want true")
	}
	if plan == nil || !plan.Unplaced {
		t.Fatalf("plan = %#v, want unplaced for 3x a100", plan)
	}

	plan, recent, err = evaluateRecentOnPremPlacement(database, placement.Constraints{GPUClass: "a100", NumGPUs: 2})
	if err != nil {
		t.Fatalf("evaluateRecentOnPremPlacement: %v", err)
	}
	if !recent {
		t.Fatal("recent = false, want true")
	}
	if plan == nil || plan.Fast == nil || plan.Fast.OnPrem == nil || plan.Fast.OnPrem.Host != "host-alpha" {
		t.Fatalf("plan = %#v, want host-alpha for 2x a100", plan)
	}
}

func newRunTestCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("provider", "", "")
	cmd.Flags().Bool("gpu-mem-strict", false, "")
	cmd.Flags().Int("disk", 0, "")
	cmd.Flags().Int("runtime-disk", 0, "")
	return cmd
}

func resetRunGlobals(t *testing.T) {
	t.Helper()
	runHost = ""
	runDir = ""
	runDescription = ""
	runProject = ""
	runDraft = false
	runFollow = false
	runWait = false
	runNoWait = false
	runKillJobID = 0
	runKillJobIDRaw = ""
	runFrom = 0
	runFromRaw = ""
	runEnvVars = nil
	runTags = nil
	runAfter = 0
	runAfterRaw = ""
	runAfterAny = 0
	runAfterAnyRaw = ""
	runGPU = ""
	runGPUCount = 0
	runGPUMem = 0
	runGPUMemStrict = false
	runInterconnect = ""
	runCPUCores = 0
	runCPUMem = 0
	runCPUMemStrict = false
	runDiskGB = 0
	runRuntimeDiskGB = 0
	runGPUClass = ""
	runCUDADriverMin = ""
	runProvider = ""
	runRunpodCloudType = ""
	runInputs = nil
	runOutputs = nil
	runProduces = nil
	runNeeds = nil
	runDryRun = false
	runNoSync = false
	runHFToken = false
	runHFTokenFrom = ""
	runSecretVars = nil
	validateRentalJobImageFunc = campaign.ValidateJobImageAvailability
}

func TestBestEffortAutoDetectedInputsExcludesExplicitRefs(t *testing.T) {
	got := bestEffortAutoDetectedInputs(
		[]string{"hf:org/explicit", "hf:gpt2", "hf:org/auto"},
		[]string{"hf:gpt2", "hf:org/auto"},
		[]string{"hf:org/explicit", "hf:gpt2"},
	)
	want := []string{"hf:org/auto"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bestEffortAutoDetectedInputs = %v, want %v", got, want)
	}
}

func TestBestEffortAutoDetectedInputsIgnoresDroppedRefs(t *testing.T) {
	got := bestEffortAutoDetectedInputs(
		[]string{"hf:org/real"},
		[]string{"hf:gpt2"},
		[]string{"hf:org/real"},
	)
	if len(got) != 0 {
		t.Fatalf("bestEffortAutoDetectedInputs = %v, want empty", got)
	}
}
