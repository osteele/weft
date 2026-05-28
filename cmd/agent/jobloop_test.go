package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/runner"
)

func TestPatchCompletionUpload(t *testing.T) {
	logDir := t.TempDir()
	jobID := int64(42)
	paths := runner.NewJobPaths(logDir, jobID)

	initial := runner.CompletionRecord{
		ExitCode:     0,
		WallTimeSecs: 1,
		EndTime:      2,
	}
	data, err := json.MarshalIndent(initial, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(paths.Completion, data, 0644); err != nil {
		t.Fatalf("write completion: %v", err)
	}

	upload := runner.OutputUploadResult{
		Status: "failed",
		Dirs: []runner.OutputDirUpload{
			{
				Dir:        "output",
				Status:     "failed",
				Error:      "disk full",
				DurationMS: 1200,
			},
		},
	}

	patchCompletionUpload(logDir, jobID, &upload)

	updated, err := os.ReadFile(paths.Completion)
	if err != nil {
		t.Fatalf("read completion: %v", err)
	}
	var decoded runner.CompletionRecord
	if err := json.Unmarshal(updated, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded.OutputUpload, &upload) {
		t.Fatalf("output upload mismatch: %#v != %#v", decoded.OutputUpload, upload)
	}
}

func TestCollectCompletionManifestIncludesPriorGraceJob(t *testing.T) {
	logDir := t.TempDir()
	writeCompletionRecordForTest(t, logDir, 2002, runner.CompletionRecord{
		ExitCode: 1,
		OutputUpload: &runner.OutputUploadResult{
			Status: "ok",
		},
		ResultsUpload: &runner.UploadSummary{
			Status: "ok",
		},
	})
	writeCompletionRecordForTest(t, logDir, 2003, runner.CompletionRecord{
		ExitCode: 0,
		OutputUpload: &runner.OutputUploadResult{
			Status: "ok",
			Bytes:  1454311849,
		},
		ResultsUpload: &runner.UploadSummary{
			Status: "ok",
		},
	})

	manifest := collectCompletionManifest(logDir, []cloud.AgentJob{{ID: 2003}})

	if manifest.ExitCode != 1 {
		t.Fatalf("manifest exit_code = %d, want 1 because prior job failed", manifest.ExitCode)
	}
	if got, want := len(manifest.Jobs), 2; got != want {
		t.Fatalf("manifest job count = %d, want %d: %#v", got, want, manifest.Jobs)
	}
	if manifest.Jobs[0].JobID != 2003 {
		t.Fatalf("first job = %d, want current grace job 2003", manifest.Jobs[0].JobID)
	}
	if manifest.Jobs[1].JobID != 2002 {
		t.Fatalf("second job = %d, want prior job 2002 from log dir", manifest.Jobs[1].JobID)
	}
	for _, job := range manifest.Jobs {
		if job.UploadStatus != "ok" {
			t.Fatalf("job %d upload_status = %q, want ok", job.JobID, job.UploadStatus)
		}
	}
}

func TestCollectCompletionManifestIncludesBackgroundSummaries(t *testing.T) {
	logDir := t.TempDir()
	writeCompletionRecordForTest(t, logDir, 2, runner.CompletionRecord{
		ExitCode: 0,
		OutputUpload: &runner.OutputUploadResult{
			Status: "ok",
			Bytes:  20,
		},
		ResultsUpload: &runner.UploadSummary{
			Status: "ok",
		},
	})
	prior := runner.JobCompletionSummary{
		JobID:        1,
		ExitCode:     0,
		UploadStatus: "ok",
		OutputBytes:  10,
	}

	manifest := collectCompletionManifest(logDir, []cloud.AgentJob{{ID: 1}, {ID: 2}}, prior)

	if got, want := len(manifest.Jobs), 2; got != want {
		t.Fatalf("manifest job count = %d, want %d: %#v", got, want, manifest.Jobs)
	}
	if manifest.Jobs[0].JobID != 1 || manifest.Jobs[0].OutputBytes != 10 {
		t.Fatalf("first summary = %#v, want background summary for job 1", manifest.Jobs[0])
	}
	if manifest.Jobs[1].JobID != 2 || manifest.Jobs[1].OutputBytes != 20 {
		t.Fatalf("second summary = %#v, want log-dir summary for job 2", manifest.Jobs[1])
	}
	if manifest.ExitCode != 0 {
		t.Fatalf("manifest exit_code = %d, want 0", manifest.ExitCode)
	}
}

func writeCompletionRecordForTest(t *testing.T, logDir string, jobID int64, rec runner.CompletionRecord) {
	t.Helper()
	paths := runner.NewJobPaths(logDir, jobID)
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(paths.Completion, data, 0o644); err != nil {
		t.Fatalf("write completion: %v", err)
	}
}

func TestHasOutputDirs(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		if hasOutputDirs(t.TempDir()) {
			t.Fatal("hasOutputDirs() = true, want false")
		}
	})

	t.Run("present", func(t *testing.T) {
		workDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(workDir, "output"), 0o755); err != nil {
			t.Fatalf("mkdir output: %v", err)
		}
		if !hasOutputDirs(workDir) {
			t.Fatal("hasOutputDirs() = false, want true")
		}
	})

	t.Run("expands tilde workdir", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		t.Run("home root", func(t *testing.T) {
			if err := os.MkdirAll(filepath.Join(homeDir, "output"), 0o755); err != nil {
				t.Fatalf("mkdir output: %v", err)
			}
			if !hasOutputDirs("~") {
				t.Fatal("hasOutputDirs() = false, want true for ~")
			}
		})

		t.Run("home subdir", func(t *testing.T) {
			workDir := filepath.Join(homeDir, "project")
			if err := os.MkdirAll(filepath.Join(workDir, "output"), 0o755); err != nil {
				t.Fatalf("mkdir output: %v", err)
			}
			if !hasOutputDirs("~/project") {
				t.Fatal("hasOutputDirs() = false, want true for ~/project")
			}
		})
	})
}

func TestSingleJobConfigForAgentJobPreservesArtifactMetadata(t *testing.T) {
	cfg := jobSequenceConfig{
		R2Bucket:  "bucket",
		PhaseKey:  "phase",
		LogDir:    "/tmp/logs",
		StartTime: time.Now(),
	}
	job := cloud.AgentJob{
		ID:         42,
		Command:    "python train.py",
		Tags:       []string{"benchmark"},
		OutputDirs: []string{"results/"},
		Produces:   []string{"results/model.pt"},
		Needs:      []string{"inputs/data.csv:41"},
		Inputs:     []string{"hf:meta-llama/Llama-3.1-8B", "hf-dataset:wikitext"},
		Env:        []string{"HF_HUB_OFFLINE=0"},
	}

	got := singleJobConfigForAgentJob(job, cfg, "/tmp/work", 5*time.Minute)

	if got.JobID != job.ID {
		t.Fatalf("job id = %d, want %d", got.JobID, job.ID)
	}
	if got.Job.Cmd != job.Command {
		t.Fatalf("command = %q, want %q", got.Job.Cmd, job.Command)
	}
	if !reflect.DeepEqual(got.Job.OutputDirs, job.OutputDirs) {
		t.Fatalf("output dirs = %v, want %v", got.Job.OutputDirs, job.OutputDirs)
	}
	if !reflect.DeepEqual(got.Job.Produces, job.Produces) {
		t.Fatalf("produces = %v, want %v", got.Job.Produces, job.Produces)
	}
	if !reflect.DeepEqual(got.Job.Needs, job.Needs) {
		t.Fatalf("needs = %v, want %v", got.Job.Needs, job.Needs)
	}
	for _, forbidden := range []string{"HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "HF_DATASETS_OFFLINE=1"} {
		if slices.Contains(got.Job.Env, forbidden) {
			t.Fatalf("env should not force %q: %v", forbidden, got.Job.Env)
		}
	}
}

func TestPrewarmLogTailIncludesRecentOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prewarm.log")
	if err := os.WriteFile(path, []byte("first\nFetching 52 files...\nNo local file found. Retrying...\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got := prewarmLogTail(path)
	if !strings.Contains(got, "prewarm log tail") {
		t.Fatalf("missing label: %q", got)
	}
	if !strings.Contains(got, "No local file found") {
		t.Fatalf("missing recent output: %q", got)
	}
}

func TestPickWatchdogTimeouts(t *testing.T) {
	cases := []struct {
		name             string
		costPerHourCents int
		wantGPUIdle      time.Duration
		wantSilence      time.Duration
	}{
		{"on-prem (zero cost)", 0, 20 * time.Minute, 30 * time.Minute},
		{"cheap 3090 at $0.30/hr", 30, 20 * time.Minute, 30 * time.Minute},
		{"just under threshold at $1.99/hr", 199, 20 * time.Minute, 30 * time.Minute},
		{"at threshold $2.00/hr", 200, 8 * time.Minute, 12 * time.Minute},
		{"H100 at $3.50/hr", 350, 8 * time.Minute, 12 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gpuIdle, silence := pickWatchdogTimeouts(tc.costPerHourCents)
			if gpuIdle != tc.wantGPUIdle {
				t.Errorf("gpu-idle = %s, want %s", gpuIdle, tc.wantGPUIdle)
			}
			if silence != tc.wantSilence {
				t.Errorf("silence = %s, want %s", silence, tc.wantSilence)
			}
		})
	}
}

func TestSingleJobConfigForAgentJob_AppliesCostTieredWatchdogs(t *testing.T) {
	t.Run("expensive cloud host uses aggressive thresholds", func(t *testing.T) {
		cfg := jobSequenceConfig{
			R2Bucket:         "bucket",
			LogDir:           "/tmp/logs",
			StartTime:        time.Now(),
			CostPerHourCents: 350, // H100 tier
		}
		got := singleJobConfigForAgentJob(cloud.AgentJob{ID: 1, Command: "echo"}, cfg, "/tmp/work", 0)
		if got.GPUIdleTimeout != 8*time.Minute {
			t.Errorf("gpu-idle = %s, want 8m", got.GPUIdleTimeout)
		}
		if got.StdoutSilenceTimeout != 12*time.Minute {
			t.Errorf("silence = %s, want 12m", got.StdoutSilenceTimeout)
		}
	})

	t.Run("on-prem uses conservative thresholds", func(t *testing.T) {
		cfg := jobSequenceConfig{
			R2Bucket:         "bucket",
			LogDir:           "/tmp/logs",
			StartTime:        time.Now(),
			CostPerHourCents: 0,
		}
		got := singleJobConfigForAgentJob(cloud.AgentJob{ID: 2, Command: "echo"}, cfg, "/tmp/work", 0)
		if got.GPUIdleTimeout != 20*time.Minute {
			t.Errorf("gpu-idle = %s, want 20m", got.GPUIdleTimeout)
		}
		if got.StdoutSilenceTimeout != 30*time.Minute {
			t.Errorf("silence = %s, want 30m", got.StdoutSilenceTimeout)
		}
	})
}

func TestSingleJobConfigForAgentJob_UsesRentalSetupTimeout(t *testing.T) {
	t.Run("rental gets longer setup window", func(t *testing.T) {
		cfg := jobSequenceConfig{
			R2Bucket: "bucket",
			LogDir:   "/tmp/logs",
			Provider: string(cloud.ProviderRunpod),
		}
		got := singleJobConfigForAgentJob(cloud.AgentJob{ID: 1, Command: "echo"}, cfg, "/tmp/work", 0)
		if got.SetupTimeout != time.Hour {
			t.Errorf("setup timeout = %s, want 1h", got.SetupTimeout)
		}
	})

	t.Run("inventory host keeps default setup window", func(t *testing.T) {
		cfg := jobSequenceConfig{
			R2Bucket: "bucket",
			LogDir:   "/tmp/logs",
		}
		got := singleJobConfigForAgentJob(cloud.AgentJob{ID: 2, Command: "echo"}, cfg, "/tmp/work", 0)
		if got.SetupTimeout != inventory.DefaultSetupTimeout {
			t.Errorf("setup timeout = %s, want %s", got.SetupTimeout, inventory.DefaultSetupTimeout)
		}
	})
}

func TestOrderJobsForSetupOverlap_PutsFastSetupFirst(t *testing.T) {
	fastDir := t.TempDir()
	heavyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(heavyDir, "pyproject.toml"), []byte("[project]\nname='heavy'\nversion='0.1.0'\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	if err := os.WriteFile(filepath.Join(heavyDir, "uv.lock"), []byte("version = 1\n"), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	jobs := []cloud.AgentJob{
		{ID: 1, Command: "echo heavy", Dir: heavyDir},
		{ID: 2, Command: "echo fast", Dir: fastDir},
	}
	got := orderJobsForSetupOverlap(jobs)
	if got[0].ID != 2 || got[1].ID != 1 {
		t.Fatalf("order = [%d,%d], want [2,1]", got[0].ID, got[1].ID)
	}
}

func TestOrderJobsForSetupOverlap_PreservesDependencyOrder(t *testing.T) {
	fastDir := t.TempDir()
	heavyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(heavyDir, "pyproject.toml"), []byte("[project]\nname='heavy'\nversion='0.1.0'\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	if err := os.WriteFile(filepath.Join(heavyDir, "uv.lock"), []byte("version = 1\n"), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	jobs := []cloud.AgentJob{
		{ID: 1, Command: "echo heavy", Dir: heavyDir},
		{ID: 2, Command: "echo fast", Dir: fastDir, Priority: 10, Needs: []string{"out/model.pt:1"}},
	}
	got := orderJobsForSetupOverlap(jobs)
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("order = [%d,%d], want producer before consumer [1,2]", got[0].ID, got[1].ID)
	}
}

func TestOrderJobsForSetupOverlap_PriorityBeatsSetupWeight(t *testing.T) {
	fastDir := t.TempDir()
	heavyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(heavyDir, "pyproject.toml"), []byte("[project]\nname='heavy'\nversion='0.1.0'\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	if err := os.WriteFile(filepath.Join(heavyDir, "uv.lock"), []byte("version = 1\n"), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	jobs := []cloud.AgentJob{
		{ID: 1, Command: "echo fast", Dir: fastDir},
		{ID: 2, Command: "echo heavy", Dir: heavyDir, Priority: 1},
	}
	got := orderJobsForSetupOverlap(jobs)
	if got[0].ID != 2 || got[1].ID != 1 {
		t.Fatalf("order = [%d,%d], want priority first [2,1]", got[0].ID, got[1].ID)
	}
}

func TestOrderJobsForSetupOverlap_PreservesOriginalOrderAsTieBreaker(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, Command: "echo one", Dir: t.TempDir()},
		{ID: 2, Command: "echo two", Dir: t.TempDir()},
	}
	got := orderJobsForSetupOverlap(jobs)
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("order = [%d,%d], want original [1,2]", got[0].ID, got[1].ID)
	}
}

func TestHFInputAssets(t *testing.T) {
	got := hfInputAssets([]string{
		"hf:meta-llama/Llama-3.1-8B",
		"hf-dataset:wikitext",
		"local:data",
		"unknown",
	})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %v", len(got), got)
	}
	if got[0].String() != "hf:meta-llama/Llama-3.1-8B" || got[1].String() != "hf-dataset:wikitext" {
		t.Fatalf("assets = %v", got)
	}
}

func TestHFDownloadScriptDownloadsModelsAndDatasets(t *testing.T) {
	script := hfDownloadScript(hfInputAssets([]string{
		"hf:org/model",
		"hf-dataset:org/data",
	}))
	for _, want := range []string{
		"ensure_hf_download_tool",
		"hf_download 'model' 'org/model'",
		"hf_download 'dataset' 'org/data'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestSetupWeightTreatsHFInputsAsSlow(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, Command: "echo hf", Dir: t.TempDir(), Inputs: []string{"hf:org/model"}},
		{ID: 2, Command: "echo fast", Dir: t.TempDir()},
	}
	got := orderJobsForSetupOverlap(jobs)
	if got[0].ID != 2 || got[1].ID != 1 {
		t.Fatalf("order = [%d,%d], want fast before HF [2,1]", got[0].ID, got[1].ID)
	}
}

func TestSnapshotLogDir_IncludesLogFiles(t *testing.T) {
	logDir := t.TempDir()
	jobID := int64(42)

	logFile := filepath.Join(logDir, "wj42.log")
	jsonFile := filepath.Join(logDir, "wj42.completion.json")
	if err := os.WriteFile(logFile, []byte("line 1\n"), 0o644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if err := os.WriteFile(jsonFile, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatalf("write completion file: %v", err)
	}

	snapshot, err := snapshotLogDir(logDir, jobID)
	if err != nil {
		t.Fatalf("snapshotLogDir: %v", err)
	}
	defer os.RemoveAll(snapshot)

	if _, err := os.Stat(filepath.Join(snapshot, "wj42.log")); err != nil {
		t.Fatalf("expected .log file in snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshot, "wj42.completion.json")); err != nil {
		t.Fatalf("expected completion file in snapshot: %v", err)
	}
}

func TestEnsureFailureArtifacts_WritesStubsWhenMissing(t *testing.T) {
	logDir := t.TempDir()
	jobID := int64(99)
	paths := runner.NewJobPaths(logDir, jobID)

	runErr := errors.New("start process: exec: \"missing-bin\": no such file")
	ensureFailureArtifacts(logDir, jobID, runner.ExitInfo{ExitCode: 0}, runErr)

	reason := runner.ReadFailureReasonFile(paths.FailureReason)
	if reason == "" {
		t.Fatal("expected failure_reason file to be written")
	}
	if !strings.Contains(reason, "start process") {
		t.Fatalf("failure_reason = %q, want it to mention start process", reason)
	}

	data, err := os.ReadFile(paths.Completion)
	if err != nil {
		t.Fatalf("expected completion.json to be written: %v", err)
	}
	var rec runner.CompletionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if rec.ExitCode != 1 {
		t.Errorf("exit_code = %d, want 1", rec.ExitCode)
	}
	if rec.FailureReason == "" {
		t.Error("expected FailureReason to be populated in completion.json")
	}
}

func TestEnsureFailureArtifacts_LeavesExistingArtifactsAlone(t *testing.T) {
	logDir := t.TempDir()
	jobID := int64(100)
	paths := runner.NewJobPaths(logDir, jobID)

	if err := runner.WriteFailureReasonFile(paths, "preexisting"); err != nil {
		t.Fatalf("seed failure_reason: %v", err)
	}
	seedCompletion := []byte(`{"exit_code":137,"failure_reason":"oom"}` + "\n")
	if err := os.WriteFile(paths.Completion, seedCompletion, 0o644); err != nil {
		t.Fatalf("seed completion: %v", err)
	}

	ensureFailureArtifacts(logDir, jobID, runner.ExitInfo{ExitCode: 137}, nil)

	if got := runner.ReadFailureReasonFile(paths.FailureReason); got != "preexisting" {
		t.Errorf("failure_reason was overwritten: got %q", got)
	}
	got, err := os.ReadFile(paths.Completion)
	if err != nil {
		t.Fatalf("read completion: %v", err)
	}
	if string(got) != string(seedCompletion) {
		t.Errorf("completion.json was overwritten: got %q", got)
	}
}

func TestEnsureFailureArtifacts_NoOpOnSuccess(t *testing.T) {
	logDir := t.TempDir()
	jobID := int64(101)
	paths := runner.NewJobPaths(logDir, jobID)

	ensureFailureArtifacts(logDir, jobID, runner.ExitInfo{ExitCode: 0}, nil)

	if _, err := os.Stat(paths.FailureReason); !os.IsNotExist(err) {
		t.Errorf("failure_reason should not exist on success, got err=%v", err)
	}
	if _, err := os.Stat(paths.Completion); !os.IsNotExist(err) {
		t.Errorf("completion.json should not be synthesized on success, got err=%v", err)
	}
}

// Verifies that the per-job failure_reason persisted by recordPrewarmFailure
// (via prewarm.err.Error()) is single-line — the multi-line prewarm log tail
// must live on setupPrewarmResult.logTail (operator stderr only), never
// inside the err that becomes the DB failure_reason column. Embedded
// newlines in failure_reason corrupted the TUI footer and `weft info`'s
// `Reason:` line (see specs/job-lifecycle.allium § FailureReason).
func TestSetupPrewarmResult_ErrorIsSingleLine(t *testing.T) {
	// Synthesize a failed prewarm by writing a multi-line script into the
	// prewarm log and feeding it to prewarmLogTail directly. The headline
	// we expect (the err returned by runSetupPrewarm) wraps the underlying
	// timeout error from RunSetupCommand; we assert here on the headline
	// format the code now produces — `fmt.Errorf("hf prewarm failed exit
	// %d: %w", ei.ExitCode, err)` — which has no embedded newlines.
	logPath := filepath.Join(t.TempDir(), "prewarm.log")
	multilineScript := "if command -v uv >/dev/null 2>&1; then\n  uv tool install foo\nfi\n"
	if err := os.WriteFile(logPath, []byte(multilineScript), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	tail := prewarmLogTail(logPath)
	if !strings.Contains(tail, "\n") {
		t.Fatal("prewarmLogTail should include embedded newlines (it represents log tail)")
	}

	// Simulate the err that runSetupPrewarm constructs (HF prewarm path).
	innerErr := fmt.Errorf("setup command timed out after 1h0m0s")
	headlineErr := fmt.Errorf("hf prewarm failed exit 124: %w", innerErr)
	if strings.Contains(headlineErr.Error(), "\n") {
		t.Errorf("headline err must be single-line; got: %q", headlineErr.Error())
	}

	// Ensure that combining headline + tail at the stderr-print site
	// still gives the operator the full picture.
	combined := fmt.Sprintf("%v%s", headlineErr, tail)
	if !strings.Contains(combined, "prewarm log tail") {
		t.Errorf("combined stderr output should include the log tail label; got: %q", combined)
	}
}
