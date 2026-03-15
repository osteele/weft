package ops

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestRecordQueuedJob_Basic(t *testing.T) {
	database := db.SetupTestDB(t)

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp/project",
		Command:     "echo hello",
		Description: "test job",
		EnvVars:     []string{"FOO=bar"},
		Tags:        []string{"gpu"},
	}

	jobID, err := RecordQueuedJob(database, params)
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}
	if jobID <= 0 {
		t.Fatalf("expected positive job ID, got %d", jobID)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Errorf("expected status=queued, got %q", job.Status)
	}
	if job.Host != "test-host" {
		t.Errorf("expected host=test-host, got %q", job.Host)
	}
	if job.Command != "echo hello" {
		t.Errorf("expected command='echo hello', got %q", job.Command)
	}
	if len(job.Tags) != 1 || job.Tags[0] != "gpu" {
		t.Errorf("expected tags=[gpu], got %v", job.Tags)
	}
}

func TestRecordQueuedJob_GPUFromEnvVars(t *testing.T) {
	database := db.SetupTestDB(t)

	params := QueueJobParams{
		Host:       "test-host",
		WorkingDir: "/tmp",
		Command:    "train.py",
		EnvVars:    []string{"CUDA_VISIBLE_DEVICES=0,1"},
	}

	jobID, err := RecordQueuedJob(database, params)
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPU != "0,1" {
		t.Errorf("expected GPU=0,1, got %q", job.GPU)
	}
}

func TestRecordQueuedJob_DefaultGPUMem(t *testing.T) {
	database := db.SetupTestDB(t)

	params := QueueJobParams{
		Host:       "test-host",
		WorkingDir: "/tmp",
		Command:    "train.py",
		GPUClass:   "a100",
	}

	jobID, err := RecordQueuedJob(database, params)
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != DefaultGPUMemGB {
		t.Errorf("expected GPUMemGB=%d, got %v", DefaultGPUMemGB, job.GPUMemGB)
	}
}

func TestRecordQueuedJob_NoSSHCalls(t *testing.T) {
	database := db.SetupTestDB(t)

	// No SSH mock — this should work purely with the DB
	params := QueueJobParams{
		Host:       "unreachable-host",
		WorkingDir: "/tmp",
		Command:    "echo test",
	}

	jobID, err := RecordQueuedJob(database, params)
	if err != nil {
		t.Fatalf("RecordQueuedJob should not require SSH, got: %v", err)
	}
	if jobID <= 0 {
		t.Fatalf("expected positive job ID, got %d", jobID)
	}

	// Job should have no LastSyncedStatus (not yet pushed)
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LastSyncedStatus != "" {
		t.Errorf("expected empty LastSyncedStatus, got %q", job.LastSyncedStatus)
	}
}

func TestRecordQueuedJob_ExplicitProject(t *testing.T) {
	database := db.SetupTestDB(t)

	params := QueueJobParams{
		Host:       "test-host",
		WorkingDir: "/tmp/project/subdir",
		Command:    "echo test",
		Project:    "explicit-project",
	}

	jobID, err := RecordQueuedJob(database, params)
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Project != "explicit-project" {
		t.Errorf("expected Project=%q, got %q", "explicit-project", job.Project)
	}
}

func TestRecordQueuedJob_DotProjectFallsBackToDirectory(t *testing.T) {
	database := db.SetupTestDB(t)

	params := QueueJobParams{
		Host:       "",
		WorkingDir: "/Users/osteele/code/research/adaptive-escalation",
		Command:    "uv run python scripts/run_backtracking_search.py --resume",
		Project:    ".",
		Tags:       []string{"cloud"},
	}

	jobID, err := RecordQueuedJob(database, params)
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Project != "adaptive-escalation" {
		t.Errorf("expected Project=%q, got %q", "adaptive-escalation", job.Project)
	}
}
