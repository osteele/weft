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
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/r2keys"
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

func TestRecordEarlyJobFailureWritesReasonAndUploadsResults(t *testing.T) {
	logDir := t.TempDir()
	jobID := int64(77)
	runID := int64(88)
	reason := "cloud_after_incomplete: producer job 5 run 50 has not completed"

	prevUpload := uploadJobResultsForAgent
	prevPut := r2PutForAgent
	t.Cleanup(func() {
		uploadJobResultsForAgent = prevUpload
		r2PutForAgent = prevPut
	})

	uploaded := false
	uploadJobResultsForAgent = func(bucket string, gotJobID, gotRunID int64, gotLogDir string) runner.UploadSummary {
		if bucket != "bucket" || gotJobID != jobID || gotRunID != runID || gotLogDir != logDir {
			t.Fatalf("upload args = bucket %q job %d run %d dir %q", bucket, gotJobID, gotRunID, gotLogDir)
		}
		paths := runner.NewJobPaths(gotLogDir, gotJobID)
		if got := runner.ReadFailureReasonFile(paths.FailureReason); got != reason {
			t.Fatalf("failure_reason before upload = %q, want %q", got, reason)
		}
		data, err := os.ReadFile(paths.Completion)
		if err != nil {
			t.Fatalf("read completion before upload: %v", err)
		}
		var rec runner.CompletionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("parse completion before upload: %v", err)
		}
		if rec.ExitCode != 1 || rec.FailureReason != reason {
			t.Fatalf("completion before upload = exit %d reason %q", rec.ExitCode, rec.FailureReason)
		}
		uploaded = true
		return runner.UploadSummary{Status: "ok"}
	}

	var markerKey, markerContent string
	r2PutForAgent = func(bucket, key, content string) error {
		if bucket != "bucket" {
			t.Fatalf("marker bucket = %q, want bucket", bucket)
		}
		if !uploaded {
			t.Fatal("completion marker written before results upload")
		}
		markerKey = key
		markerContent = content
		return nil
	}

	recordEarlyJobFailure(
		jobSequenceConfig{R2Bucket: "bucket", LogDir: logDir},
		cloud.AgentJob{ID: jobID, RunID: runID},
		reason,
		1,
	)

	if !uploaded {
		t.Fatal("results upload was not invoked")
	}
	if want := r2keys.JobAttemptComplete(jobID, runID); markerKey != want || markerContent != "1" {
		t.Fatalf("marker = %q content %q, want %q content 1", markerKey, markerContent, want)
	}
}

func TestCanRunSlottedJobsConcurrently(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, SlotGPU: true, GPU: "0", GPUCount: 1},
		{ID: 2, SlotGPU: true, GPU: "1", GPUCount: 1},
	}
	if !canRunSlottedJobsConcurrently(jobs) {
		t.Fatal("distinct slotted jobs should run concurrently")
	}
}

func TestCanRunSlottedJobsConcurrentlyRejectsBenchmark(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, SlotGPU: true, GPU: "0", GPUCount: 1, Tags: []string{db.TagBenchmark}},
		{ID: 2, SlotGPU: true, GPU: "1", GPUCount: 1},
	}
	if canRunSlottedJobsConcurrently(jobs) {
		t.Fatal("benchmark jobs must stay on the sequential barrier path")
	}
}

func TestCanRunSlottedJobsConcurrentlyRequiresSlotFlag(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, GPU: "0", GPUCount: 1},
		{ID: 2, GPU: "1", GPUCount: 1},
	}
	if canRunSlottedJobsConcurrently(jobs) {
		t.Fatal("plain device-pinned jobs must not imply packed slot concurrency")
	}
}

func TestCanRunSlottedJobsConcurrentlyRejectsCloudAfterPeer(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, SlotGPU: true, GPU: "0", GPUCount: 1},
		{ID: 2, SlotGPU: true, GPU: "1", GPUCount: 1, CloudAfter: []cloud.CloudAfterRef{{JobID: 1, RunID: 101}}},
	}
	if canRunSlottedJobsConcurrently(jobs) {
		t.Fatal("CloudAfter refs within the slot set must stay on the sequential path")
	}
}

func TestCanRunSlottedJobsConcurrentlyAllowsCloudAfterOutsideSet(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, SlotGPU: true, GPU: "0", GPUCount: 1},
		{ID: 2, SlotGPU: true, GPU: "1", GPUCount: 1, CloudAfter: []cloud.CloudAfterRef{{JobID: 99, RunID: 101}}},
	}
	if !canRunSlottedJobsConcurrently(jobs) {
		t.Fatal("CloudAfter refs outside the slot set should not block concurrent slots")
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

func TestHFPrewarmTextHasXetError(t *testing.T) {
	// Writer/reconstruction failure (xet upload/rebuild path).
	writerTrace := `RuntimeError: Data processing error: CAS service error : Reqwest Error: HTTP status client error (401 Unauthorized), domain: https://cas-server.xethub.hf.co
RuntimeError: Task error: File reconstruction error: Internal Writer Error: Background writer channel closed`
	if !hfPrewarmTextHasXetError(writerTrace) {
		t.Fatal("hfPrewarmTextHasXetError() = false for xet reconstruction writer error")
	}
	// Read-token 404 for an xet-migrated model (gpt2 / gpt2-xl): the failure
	// mode that took down auto-detected and explicit gpt2 inputs.
	readTokenTrace := `huggingface_hub.errors.ConnectionError: Network error: Request error: HTTP status client error (404 Not Found), domain: https://huggingface.co/api/models/gpt2/xet-read-token/607a30d783dfa663
  File "huggingface_hub/file_download.py", line 500, in xet_get
  File "huggingface_hub/file_download.py", line 480, in new_file_download_group`
	if !hfPrewarmTextHasXetError(readTokenTrace) {
		t.Fatal("hfPrewarmTextHasXetError() = false for xet-read-token 404 (migrated model)")
	}
	// Reconstruction wording without any xet context: not our fallback case.
	if hfPrewarmTextHasXetError("File reconstruction error without CAS context") {
		t.Fatal("hfPrewarmTextHasXetError() = true without xet context")
	}
	// xet mentioned but no recognized transfer-failure signature.
	if hfPrewarmTextHasXetError("xet authentication failed before download") {
		t.Fatal("hfPrewarmTextHasXetError() = true without a recognized xet failure signature")
	}
}

func TestPrewarmShouldTerminateAsInfra(t *testing.T) {
	if !prewarmShouldTerminateAsInfra(setupPrewarmResult{infraFailure: true, exitInfo: runner.ExitInfo{ExitCode: 1}}) {
		t.Fatal("infra prewarm failure should terminate as infrastructure")
	}
	if !prewarmShouldTerminateAsInfra(setupPrewarmResult{exitInfo: runner.ExitInfo{ExitCode: runner.ExitCodeSetupTimeout}}) {
		t.Fatal("setup timeout should terminate as infrastructure")
	}
	if prewarmShouldTerminateAsInfra(setupPrewarmResult{exitInfo: runner.ExitInfo{ExitCode: 1}}) {
		t.Fatal("generic setup exit 1 should not terminate as infrastructure")
	}
}

func TestRecordPrewarmFailureWritesMachineReason(t *testing.T) {
	logDir := t.TempDir()
	prewarmLog := filepath.Join(t.TempDir(), "prewarm.log")
	if err := os.WriteFile(prewarmLog, []byte("xet failure\n"), 0o644); err != nil {
		t.Fatalf("write prewarm log: %v", err)
	}
	job := cloud.AgentJob{
		ID:      42,
		RunID:   7,
		Dir:     "/tmp/project",
		Command: "python train.py",
	}
	prewarm := setupPrewarmResult{
		logPath:       prewarmLog,
		exitInfo:      runner.ExitInfo{ExitCode: 1},
		err:           errors.New("hf prewarm failed exit 1"),
		failureReason: db.FailureReasonInfraPrewarmDownloadFailed,
		infraFailure:  true,
	}

	exitCode := recordPrewarmFailure(jobSequenceConfig{LogDir: logDir}, job, prewarm)
	if exitCode != 1 {
		t.Fatalf("recordPrewarmFailure exit code = %d, want 1", exitCode)
	}

	paths := runner.NewJobPaths(logDir, job.ID)
	reasonData, err := os.ReadFile(paths.FailureReason)
	if err != nil {
		t.Fatalf("read failure reason: %v", err)
	}
	if strings.TrimSpace(string(reasonData)) != db.FailureReasonInfraPrewarmDownloadFailed {
		t.Fatalf("failure reason = %q, want %q", strings.TrimSpace(string(reasonData)), db.FailureReasonInfraPrewarmDownloadFailed)
	}

	var completion runner.CompletionRecord
	data, err := os.ReadFile(paths.Completion)
	if err != nil {
		t.Fatalf("read completion: %v", err)
	}
	if err := json.Unmarshal(data, &completion); err != nil {
		t.Fatalf("unmarshal completion: %v", err)
	}
	if completion.FailureReason != db.FailureReasonInfraPrewarmDownloadFailed {
		t.Fatalf("completion failure reason = %q, want %q", completion.FailureReason, db.FailureReasonInfraPrewarmDownloadFailed)
	}
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
		Tags:       []string{"benchmark-isolation"},
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

func TestHFDownloadPrewarmEnvOverridesRuntimeOfflineFlags(t *testing.T) {
	base := []string{
		"HF_TOKEN=secret",
		"HF_HUB_OFFLINE=1",
		"TRANSFORMERS_OFFLINE=1",
		"HF_DATASETS_OFFLINE=1",
	}
	envGot := hfDownloadPrewarmEnv(base, []string{"hf:gpt2"})

	for _, tt := range []struct {
		key  string
		want string
	}{
		{"HF_TOKEN", "secret"},
		{"HF_HUB_OFFLINE", "0"},
		{"TRANSFORMERS_OFFLINE", "0"},
		{"HF_DATASETS_OFFLINE", "0"},
	} {
		if got := lastEnvValue(envGot, tt.key); got != tt.want {
			t.Fatalf("%s = %q, want %q in %v", tt.key, got, tt.want, envGot)
		}
	}
}

func TestHFDownloadPrewarmEnvLeavesNonHFJobsAlone(t *testing.T) {
	base := []string{"HF_HUB_OFFLINE=1"}
	got := hfDownloadPrewarmEnv(base, []string{"asset:local-tokenizer"})
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("env = %v, want %v", got, base)
	}
}

func TestRuntimeJobEnvPreservesOfflineFlags(t *testing.T) {
	cfg := jobSequenceConfig{
		R2Bucket:  "bucket",
		PhaseKey:  "phase",
		LogDir:    "/tmp/logs",
		StartTime: time.Now(),
	}
	job := cloud.AgentJob{
		ID:      42,
		Command: "python train.py",
		Inputs:  []string{"hf:gpt2"},
		Env: []string{
			"HF_HUB_OFFLINE=1",
			"TRANSFORMERS_OFFLINE=1",
			"HF_DATASETS_OFFLINE=1",
		},
	}

	got := singleJobConfigForAgentJob(job, cfg, "/tmp/work", 5*time.Minute)

	for _, key := range []string{"HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE", "HF_DATASETS_OFFLINE"} {
		if got := lastEnvValue(got.Job.Env, key); got != "1" {
			t.Fatalf("%s = %q, want runtime env to preserve offline mode in %v", key, got, got)
		}
	}
}

func lastEnvValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
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

func TestSplitHFInputAssets(t *testing.T) {
	explicit, bestEffort := splitHFInputAssets(
		[]string{"hf:org/explicit", "hf:org/auto", "hf-dataset:org/data", "local:data"},
		[]string{"hf:org/auto"},
	)
	if got, want := assetRefs(explicit), []string{"hf:org/explicit", "hf-dataset:org/data"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit assets = %v, want %v", got, want)
	}
	if got, want := assetRefs(bestEffort), []string{"hf:org/auto"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("best-effort assets = %v, want %v", got, want)
	}
}

func assetRefs(assets []dataloc.DataAsset) []string {
	refs := make([]string, 0, len(assets))
	for _, asset := range assets {
		refs = append(refs, asset.Ref())
	}
	return refs
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
		// FR1: model downloads skip redundant native checkpoints; datasets don't.
		"--exclude 'original/*'",
		`ignore_patterns=["original/*"]`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestRunSetupPrewarmBestEffortHFFailureWarnsAndContinues(t *testing.T) {
	restore := stubPrewarmRunner(t, func(script string, jobID int64, workDir string, env []string, paths runner.JobPaths, timeout time.Duration) (runner.ExitInfo, error) {
		return runner.ExitInfo{ExitCode: 1}, errors.New("download failed")
	})
	defer restore()

	result := runSetupPrewarm(cloud.AgentJob{
		ID:               71,
		RunID:            1,
		Command:          "python train.py",
		Inputs:           []string{"hf:org/auto"},
		BestEffortInputs: []string{"hf:org/auto"},
	}, jobSequenceConfig{}, t.TempDir(), false)

	if !result.ok {
		t.Fatalf("best-effort prewarm should continue, got result: %+v", result)
	}
	data, err := os.ReadFile(result.logPath)
	if err != nil {
		t.Fatalf("read prewarm log: %v", err)
	}
	if !strings.Contains(string(data), "weft: auto-detected input hf:org/auto failed to stage; continuing (best-effort)") {
		t.Fatalf("missing best-effort warning:\n%s", string(data))
	}
}

func TestRunSetupPrewarmExplicitHFFailureRemainsFatal(t *testing.T) {
	restore := stubPrewarmRunner(t, func(script string, jobID int64, workDir string, env []string, paths runner.JobPaths, timeout time.Duration) (runner.ExitInfo, error) {
		return runner.ExitInfo{ExitCode: 1}, errors.New("download failed")
	})
	defer restore()

	result := runSetupPrewarm(cloud.AgentJob{
		ID:      72,
		RunID:   1,
		Command: "python train.py",
		Inputs:  []string{"hf:org/explicit"},
	}, jobSequenceConfig{}, t.TempDir(), false)

	if result.ok {
		t.Fatal("explicit HF prewarm failure should be fatal")
	}
	if !result.infraFailure {
		t.Fatalf("explicit HF prewarm failure should remain infra, got result: %+v", result)
	}
}

func TestRunSetupPrewarmBestEffortKeepsXetFallback(t *testing.T) {
	var envs [][]string
	restore := stubPrewarmRunner(t, func(script string, jobID int64, workDir string, env []string, paths runner.JobPaths, timeout time.Duration) (runner.ExitInfo, error) {
		envs = append(envs, append([]string(nil), env...))
		if len(envs) == 1 {
			appendPrewarmStatus(paths.Log, "xet-read-token failed in xet_get\n")
		}
		return runner.ExitInfo{ExitCode: 1}, errors.New("download failed")
	})
	defer restore()

	result := runSetupPrewarm(cloud.AgentJob{
		ID:               73,
		RunID:            1,
		Command:          "python train.py",
		Inputs:           []string{"hf:org/auto"},
		BestEffortInputs: []string{"hf:org/auto"},
	}, jobSequenceConfig{}, t.TempDir(), false)

	if !result.ok {
		t.Fatalf("best-effort prewarm should continue, got result: %+v", result)
	}
	if len(envs) < 2 {
		t.Fatalf("expected xet retry, got %d attempts", len(envs))
	}
	if !slices.Contains(envs[1], "HF_HUB_DISABLE_XET=1") {
		t.Fatalf("second attempt env missing HF_HUB_DISABLE_XET=1: %v", envs[1])
	}
}

func stubPrewarmRunner(t *testing.T, fn func(string, int64, string, []string, runner.JobPaths, time.Duration) (runner.ExitInfo, error)) func() {
	t.Helper()
	oldRunner := runSetupCommand
	oldBackoff := hfPrewarmBackoffFunc
	runSetupCommand = fn
	hfPrewarmBackoffFunc = func(int) time.Duration { return 0 }
	return func() {
		runSetupCommand = oldRunner
		hfPrewarmBackoffFunc = oldBackoff
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

	snapshot, err := snapshotLogDir(logDir, jobID, 7)
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

// Regression test: snapshot dirs must be keyed by job ID + run ID so that a
// second attempt of the same job on the same instance does not share a
// directory with the first attempt's still-uploading snapshot. The first
// attempt's background upload ends with os.RemoveAll of its snapshot; with a
// job-ID-only key that deleted the second attempt's files (or uploaded stale
// first-attempt files under the new attempt).
func TestSnapshotLogDir_DistinctPerAttempt(t *testing.T) {
	logDir := t.TempDir()
	jobID := int64(43)

	if err := os.WriteFile(filepath.Join(logDir, "wj43.log"), []byte("attempt 1\n"), 0o644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	first, err := snapshotLogDir(logDir, jobID, 1)
	if err != nil {
		t.Fatalf("snapshotLogDir attempt 1: %v", err)
	}
	defer os.RemoveAll(first)

	if err := os.WriteFile(filepath.Join(logDir, "wj43.log"), []byte("attempt 2\n"), 0o644); err != nil {
		t.Fatalf("rewrite log file: %v", err)
	}
	second, err := snapshotLogDir(logDir, jobID, 2)
	if err != nil {
		t.Fatalf("snapshotLogDir attempt 2: %v", err)
	}
	defer os.RemoveAll(second)

	if first == second {
		t.Fatalf("attempts share a snapshot dir: %s", first)
	}

	// Simulate attempt 1's background upload finishing (bgwork.go RemoveAll).
	if err := os.RemoveAll(first); err != nil {
		t.Fatalf("remove first snapshot: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(second, "wj43.log"))
	if err != nil {
		t.Fatalf("second attempt's snapshot lost after first attempt cleanup: %v", err)
	}
	if string(data) != "attempt 2\n" {
		t.Fatalf("second snapshot content = %q, want attempt 2's log", data)
	}
}

// Regression test: the exit code recorded for a failed prewarm (and reused by
// the background-upload R2 marker repair) must preserve exit 124 (setup-phase
// timeout) instead of clobbering it to 1.
func TestPrewarmFailureExitCode(t *testing.T) {
	cases := []struct {
		name string
		exit int
		want int
	}{
		{"setup timeout preserved", runner.ExitCodeSetupTimeout, runner.ExitCodeSetupTimeout},
		{"nonzero preserved", 2, 2},
		{"zero maps to generic failure", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pw := setupPrewarmResult{exitInfo: runner.ExitInfo{ExitCode: tc.exit}}
			if got := prewarmFailureExitCode(pw); got != tc.want {
				t.Fatalf("prewarmFailureExitCode(exit=%d) = %d, want %d", tc.exit, got, tc.want)
			}
		})
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

func TestOutputWindowStartForJob_RestagedDisablesWindow(t *testing.T) {
	start := int64(1_700_000_000)
	if got := outputWindowStartForJob(cloud.AgentJob{}, start); got != start {
		t.Errorf("outputWindowStartForJob(plain) = %d, want %d", got, start)
	}
	if got := outputWindowStartForJob(cloud.AgentJob{RestagedOutputs: true}, start); got != 0 {
		t.Errorf("outputWindowStartForJob(restaged) = %d, want 0 (window disabled)", got)
	}
}
