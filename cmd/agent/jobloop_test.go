package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
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
