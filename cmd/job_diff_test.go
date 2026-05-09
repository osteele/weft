package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestNormalizeJobRecordRedactsSecretEnv(t *testing.T) {
	job := &db.Job{
		ID:      42,
		Status:  db.StatusQueued,
		EnvVars: []string{"HF_TOKEN=secret:hf", "WANDB_API_KEY=abc", "HF_HOME=/tmp/hf"},
		Inputs:  []string{"hf:gpt2"},
	}
	rec := normalizeJobRecord(job, nil)

	got := strings.Join(rec.Env, "|")
	if strings.Contains(got, "secret:hf") || strings.Contains(got, "abc") {
		t.Fatalf("env was not redacted: %v", rec.Env)
	}
	if !strings.Contains(got, "HF_TOKEN=<redacted>") || !strings.Contains(got, "WANDB_API_KEY=<redacted>") {
		t.Fatalf("missing redacted keys: %v", rec.Env)
	}
	if !strings.Contains(got, "HF_HOME=/tmp/hf") {
		t.Fatalf("non-secret env should be preserved: %v", rec.Env)
	}
}

func TestDiffNormalizedJobsIncludesBehavioralFields(t *testing.T) {
	a := normalizeJobRecord(&db.Job{
		ID:       1876,
		Host:     "cool100",
		Command:  "uv run python train.py",
		Inputs:   []string{"hf:gpt2"},
		EnvVars:  []string{"HF_HOME=$TMPDIR/hf-cache"},
		GPUClass: "ampere+",
	}, nil)
	b := normalizeJobRecord(&db.Job{
		ID:       1877,
		Host:     "cool100",
		Command:  "uv run python train.py",
		Inputs:   []string{"hf:gpt2"},
		EnvVars:  []string{"HF_HOME=/project/cache/hf"},
		GPUClass: "ampere+",
	}, nil)

	diff := diffNormalizedJobs(a, b)
	if len(diff.Changes) != 1 {
		t.Fatalf("changes = %#v, want exactly env change", diff.Changes)
	}
	if diff.Changes[0].Field != "env" {
		t.Fatalf("field = %q, want env", diff.Changes[0].Field)
	}
}
