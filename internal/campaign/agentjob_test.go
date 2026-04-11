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
		ID:          42,
		Command:     "python train.py",
		Tags:        []string{"benchmark", "nightly"},
		GPUClass:    "a100",
		OutputDirs:  []string{"results/"},
		Produces:    []string{"results/model.pt"},
		Needs:       []string{"inputs/data.csv:41"},
		EnvVars:     []string{"UV_INDEX_URL=https://example.com/simple", "CUDA_HOME=/usr/local/cuda"},
		LatestRunID: &runID,
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
	if !reflect.DeepEqual(got.OutputDirs, job.OutputDirs) {
		t.Fatalf("output dirs = %v, want %v", got.OutputDirs, job.OutputDirs)
	}
	if !reflect.DeepEqual(got.Produces, job.Produces) {
		t.Fatalf("produces = %v, want %v", got.Produces, job.Produces)
	}
	if !reflect.DeepEqual(got.Needs, job.Needs) {
		t.Fatalf("needs = %v, want %v", got.Needs, job.Needs)
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
