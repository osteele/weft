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

func TestRecordQueuedJobRejectsExplicitHFModelAlias(t *testing.T) {
	database := db.SetupTestDB(t)

	_, err := RecordQueuedJob(database, QueueJobParams{
		WorkingDir: "/tmp/project",
		Command:    "python train.py",
		Inputs:     []string{"hf:llama-3.1-8b"},
	})
	if err == nil {
		t.Fatal("expected input validation error")
	}
	if !strings.Contains(err.Error(), "hf:meta-llama/Llama-3.1-8B") {
		t.Fatalf("error should suggest canonical repo, got %v", err)
	}
	jobs, listErr := db.ListJobsWithMaxAge(database, "", "", 10, 0, nil, "")
	if listErr != nil {
		t.Fatalf("ListJobsWithMaxAge: %v", listErr)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs len = %d, want 0", len(jobs))
	}
}

func TestRecordQueuedJob_PersistsSubmitMetadataAtomically(t *testing.T) {
	database := db.SetupTestDB(t)

	gpuMem := 24
	overrides := &db.CLIResourceOverrides{
		GPUClass: "ampere+",
		GPUMemGB: &gpuMem,
	}
	jobID, err := RecordQueuedJob(database, QueueJobParams{
		WorkingDir:    "/tmp/project",
		Command:       "python train.py",
		Description:   "metadata-rich submit",
		CLIOverrides:  overrides,
		MaxComputeCap: "12.0",
		SubmitToken:   "submit-token-1",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.GPUClass != "ampere+" {
		t.Fatalf("CLI overrides = %#v, want gpu class", job.CLIResourceOverrides)
	}
	if job.MaxComputeCap != "12.0" {
		t.Fatalf("MaxComputeCap = %q, want 12.0", job.MaxComputeCap)
	}
	gotID, ok, err := db.FindJobIDBySubmitToken(database, "submit-token-1")
	if err != nil {
		t.Fatalf("FindJobIDBySubmitToken: %v", err)
	}
	if !ok || gotID != jobID {
		t.Fatalf("submit token lookup = %d,%v; want %d,true", gotID, ok, jobID)
	}
}

func TestRecordQueuedJob_SubmitTokenReturnsExistingJob(t *testing.T) {
	database := db.SetupTestDB(t)

	params := QueueJobParams{
		WorkingDir:  "/tmp/project",
		Command:     "python first.py",
		Description: "first",
		SubmitToken: "submit-token-2",
	}
	firstID, err := RecordQueuedJob(database, params)
	if err != nil {
		t.Fatalf("first RecordQueuedJob: %v", err)
	}
	params.Command = "python duplicate.py"
	secondID, err := RecordQueuedJob(database, params)
	if err != nil {
		t.Fatalf("second RecordQueuedJob: %v", err)
	}
	if secondID != firstID {
		t.Fatalf("duplicate submit token returned job %d, want %d", secondID, firstID)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM jobs WHERE submit_token = ?`, params.SubmitToken).Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("jobs with submit token = %d, want 1", count)
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

func TestRecordQueuedJob_RejectsMalformedNeedsBeforePersisting(t *testing.T) {
	database := db.SetupTestDB(t)

	_, err := RecordQueuedJob(database, QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp/project",
		Command:     "python consume.py",
		Description: "bad needs submit",
		Needs:       []string{"output/nsweep_reps.tar:wj5070"},
	})
	if err == nil {
		t.Fatal("expected malformed needs to be rejected")
	}
	if !strings.Contains(err.Error(), `needs spec "output/nsweep_reps.tar:wj5070"`) {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM jobs WHERE command = ?`, "python consume.py").Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("persisted jobs = %d, want 0", count)
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
