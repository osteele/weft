package campaign

import (
	"reflect"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestNewAgentJobPreservesArtifactMetadata(t *testing.T) {
	runID := int64(88)
	job := &db.Job{
		ID:               42,
		Command:          "python train.py",
		Tags:             []string{"benchmark-isolation", "nightly"},
		GPUClass:         "a100",
		Priority:         3,
		OutputDirs:       []string{"results/"},
		Produces:         []string{"results/model.pt"},
		Needs:            []string{"inputs/data.csv:41"},
		Inputs:           []string{"hf:org/explicit", "hf:org/auto"},
		BestEffortInputs: []string{"hf:org/auto"},
		EnvVars:          []string{"UV_INDEX_URL=https://example.com/simple", "CUDA_HOME=/usr/local/cuda"},
		LatestRunID:      &runID,
	}

	got := newAgentJob(job, cloud.ProjectRootDir+"/repo")

	if got.ID != job.ID || got.RunID != runID {
		t.Fatalf("agent job ids = (%d, %d), want (%d, %d)", got.ID, got.RunID, job.ID, runID)
	}
	if got.Command != job.Command {
		t.Fatalf("command = %q, want %q", got.Command, job.Command)
	}
	if got.Dir != cloud.ProjectRootDir+"/repo" {
		t.Fatalf("dir = %q", got.Dir)
	}
	if got.Priority != job.Priority {
		t.Fatalf("priority = %d, want %d", got.Priority, job.Priority)
	}
	if !reflect.DeepEqual(got.OutputDirs, job.OutputDirs) {
		t.Fatalf("output dirs = %v, want %v", got.OutputDirs, job.OutputDirs)
	}
	if !reflect.DeepEqual(got.Produces, job.Produces) {
		t.Fatalf("produces = %v, want %v", got.Produces, job.Produces)
	}
	if !reflect.DeepEqual(got.Needs, job.Needs) {
		t.Fatalf("needs = %v, want %v", got.Needs, job.Needs)
	}
	if !reflect.DeepEqual(got.Inputs, job.Inputs) {
		t.Fatalf("inputs = %v, want %v", got.Inputs, job.Inputs)
	}
	if !reflect.DeepEqual(got.BestEffortInputs, job.BestEffortInputs) {
		t.Fatalf("best-effort inputs = %v, want %v", got.BestEffortInputs, job.BestEffortInputs)
	}
	if !reflect.DeepEqual(got.Env, job.EnvVars) {
		t.Fatalf("env = %v, want %v", got.Env, job.EnvVars)
	}
}

func TestNewAgentJobNilEnvVars(t *testing.T) {
	job := &db.Job{
		ID:      1,
		Command: "echo hello",
	}
	got := newAgentJob(job, "/workspace/repo")
	if got.Env != nil {
		t.Fatalf("env = %v, want nil", got.Env)
	}
}

func TestNewCloudAgentJobRejectsMissingRunID(t *testing.T) {
	job := &db.Job{
		ID:      7,
		Command: "echo hi",
	}
	if _, err := newCloudAgentJob(job, "/workspace/repo"); err == nil {
		t.Fatal("expected error for missing latest_run_id")
	}
}

func TestAssertNeedsClassified(t *testing.T) {
	t.Run("empty needs is fine", func(t *testing.T) {
		job := &db.Job{ID: 100}
		if err := assertNeedsClassified(job, nil, nil, nil); err != nil {
			t.Fatalf("assertNeedsClassified: %v", err)
		}
	})

	t.Run("on-prem skipped spec passes", func(t *testing.T) {
		spec := "out/x.pkl:42"
		job := &db.Job{ID: 101, Needs: []string{spec}}
		if err := assertNeedsClassified(job, nil, nil, []string{spec}); err != nil {
			t.Fatalf("assertNeedsClassified: %v", err)
		}
	})

	t.Run("rental need with matching CloudNeed passes", func(t *testing.T) {
		spec := "out/x.pkl:42"
		job := &db.Job{ID: 102, Needs: []string{spec}}
		cn := []cloud.CloudNeed{{Spec: spec, Path: "out/x.pkl", R2Key: "k"}}
		if err := assertNeedsClassified(job, cn, nil, nil); err != nil {
			t.Fatalf("assertNeedsClassified: %v", err)
		}
	})

	t.Run("rental need with matching CloudAfter passes", func(t *testing.T) {
		spec := "out/x.pkl:42"
		job := &db.Job{ID: 103, Needs: []string{spec}}
		ca := []cloud.CloudAfterRef{{JobID: 42}}
		if err := assertNeedsClassified(job, nil, ca, nil); err != nil {
			t.Fatalf("assertNeedsClassified: %v", err)
		}
	})

	t.Run("rental need with empty classification fails (the wj1213 bug)", func(t *testing.T) {
		spec := "out/x.pkl:42"
		job := &db.Job{ID: 104, Needs: []string{spec}}
		err := assertNeedsClassified(job, nil, nil, nil)
		if err == nil {
			t.Fatal("expected invariant failure for unclassified rental need")
		}
	})
}
