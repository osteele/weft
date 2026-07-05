package ops

import (
	"reflect"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/opsqueue"
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

func TestRecordQueuedJob_PersistsBestEffortInputs(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := RecordQueuedJob(database, QueueJobParams{
		Host:             "test-host",
		WorkingDir:       "/tmp/project",
		Command:          "python train.py",
		Inputs:           []string{"hf:org/explicit", "hf:org/auto"},
		BestEffortInputs: []string{"hf:org/auto"},
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got, want := job.BestEffortInputs, []string{"hf:org/auto"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("BestEffortInputs = %v, want %v", got, want)
	}
	if job.Metadata == nil || !reflect.DeepEqual(job.Metadata.BestEffortInputs, []string{"hf:org/auto"}) {
		t.Fatalf("metadata best-effort inputs not persisted: %#v", job.Metadata)
	}
}

func TestRecordQueuedJob_RollsBackPartialRecordOnLateError(t *testing.T) {
	database := db.SetupTestDB(t)

	if _, err := database.Exec(`
		CREATE TEMP TRIGGER fail_dep_spec_update
		BEFORE UPDATE OF dep_spec ON jobs
		BEGIN
			SELECT RAISE(FAIL, 'forced dep_spec failure');
		END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	gpuMemGB := 30
	_, err := RecordQueuedJob(database, QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp/project",
		Command:     "python replay.py",
		Description: "metadata-rich submit",
		Tags:        []string{"benchmark", "EXP-358"},
		GPUClass:    "ampere+",
		GPUMemGB:    &gpuMemGB,
		Inputs:      []string{"hf:Qwen/Qwen2.5-7B"},
		DepSpec:     "123",
	})
	if err == nil {
		t.Fatal("expected forced dep_spec failure")
	}
	if !strings.Contains(err.Error(), "forced dep_spec failure") {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM jobs WHERE command = ?`, "python replay.py").Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("partial job row survived failed submit; count=%d", count)
	}
}

func TestRecordQueuedJob_CloudRequiresWorkingDir(t *testing.T) {
	database := db.SetupTestDB(t)

	_, err := RecordQueuedJob(database, QueueJobParams{
		Command:     "echo hello",
		Description: "cloud job",
	})
	if err == nil {
		t.Fatal("expected missing working directory error")
	}
	if !strings.Contains(err.Error(), "cloud jobs require a local working directory") {
		t.Fatalf("unexpected error: %v", err)
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
	if job.GPUMemGB == nil || *job.GPUMemGB != opsqueue.DefaultGPUMemGB {
		t.Errorf("expected GPUMemGB=%d, got %v", opsqueue.DefaultGPUMemGB, job.GPUMemGB)
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

// TestRecordQueuedJob_PersistsCloudNeedsMetadata verifies that CloudNeeds/CloudAfter
// metadata is persisted as part of RecordQueuedJob itself. Prior to this fix the
// caller was responsible for a follow-up SetJobMetadata call, which left a race
// window in which the background sync worker could pick up the freshly-queued
// job and dispatch it without staging its cloud artifacts — the wj1088 incident.
func TestRecordQueuedJob_PersistsCloudNeedsMetadata(t *testing.T) {
	database := db.SetupTestDB(t)

	meta := &db.JobMetadata{
		Dependencies: &db.JobDependencyMetadata{
			CloudNeeds: []string{"output/exp_004/ise_masked_checkpoint.pt:1046"},
			CloudAfter: []db.JobDependencyRef{{JobID: 1046}},
		},
	}
	params := QueueJobParams{
		Host:       "test-host",
		WorkingDir: "/tmp/project",
		Command:    "echo hi",
		Metadata:   meta,
	}

	jobID, err := RecordQueuedJob(database, params)
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	// Fetching the job immediately (as the sync worker would) must see the
	// cloud dependency metadata — no race window between job insert and
	// metadata write.
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Metadata == nil || job.Metadata.Dependencies == nil {
		t.Fatalf("expected metadata.Dependencies, got %+v", job.Metadata)
	}
	if got := job.Metadata.Dependencies.CloudNeeds; len(got) != 1 || got[0] != meta.Dependencies.CloudNeeds[0] {
		t.Errorf("CloudNeeds: want %v, got %v", meta.Dependencies.CloudNeeds, got)
	}
	if got := job.Metadata.Dependencies.CloudAfter; len(got) != 1 || got[0].JobID != 1046 {
		t.Errorf("CloudAfter: want JobID=1046, got %v", got)
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
