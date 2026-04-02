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
