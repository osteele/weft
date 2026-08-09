package cmd

import (
	"encoding/json"
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

func TestLoadNormalizedJobRecordIncludesAttemptPublication(t *testing.T) {
	database := db.SetupTestDB(t)
	const jobID = int64(1880)
	if _, err := database.Exec(`INSERT INTO jobs (id, command, working_dir, created_at) VALUES (?, 'true', '/tmp', 1)`, jobID); err != nil {
		t.Fatal(err)
	}
	attemptID, err := db.CreateAttempt(database, jobID, "host-alpha", nil, db.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertAttemptPublicationState(database, &db.AttemptPublicationState{
		AttemptID: attemptID, JobID: jobID, Sequence: 1, ObservedAt: 2,
		ExecutionState:         db.PublicationExecutionComplete,
		RequiredArtifactsState: db.PublicationStateReady,
		DrainState:             db.PublicationStatePending,
		UnknownReason:          "drain observation unavailable",
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := loadNormalizedJobRecord(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Publication == nil || len(rec.Attempts) != 1 || rec.Attempts[0].Publication == nil {
		t.Fatalf("normalized record omitted publication: %+v", rec)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"execution_state":"complete"`, `"required_artifacts_state":"ready"`, `"drain_state":"pending"`, `"unknown_reason":"drain observation unavailable"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("inspect JSON missing %s: %s", want, text)
		}
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
