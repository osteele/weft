package db

import (
	"database/sql"
	"errors"
	"testing"
)

func TestUpsertAndListArtifacts(t *testing.T) {
	database := setupTestDB(t)

	art := Artifact{
		JobID:      1,
		Name:       "results",
		Path:       "output/results.json",
		StoredPath: "1/output/results.json",
		SizeBytes:  123,
		SHA256:     "abc",
	}
	if err := UpsertArtifact(database, art); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	arts, err := ListArtifactsByJob(database, 1)
	if err != nil {
		t.Fatalf("ListArtifactsByJob: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}

	art.StoredPath = "1/output/results-v2.json"
	art.SizeBytes = 456
	if err := UpsertArtifact(database, art); err != nil {
		t.Fatalf("UpsertArtifact update: %v", err)
	}

	arts, err = ListArtifactsByJob(database, 1)
	if err != nil {
		t.Fatalf("ListArtifactsByJob: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact after update, got %d", len(arts))
	}
	if arts[0].StoredPath != art.StoredPath {
		t.Fatalf("expected stored_path %q, got %q", art.StoredPath, arts[0].StoredPath)
	}
}

func TestFindArtifactByNameOrPath(t *testing.T) {
	database := setupTestDB(t)

	art := Artifact{
		JobID:      2,
		Name:       "model",
		Path:       "runs/model.bin",
		StoredPath: "2/runs/model.bin",
		SizeBytes:  10,
		SHA256:     "deadbeef",
	}
	if err := UpsertArtifact(database, art); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	if _, err := FindArtifactByNameOrPath(database, 2, "model"); err != nil {
		t.Fatalf("FindArtifactByNameOrPath by name: %v", err)
	}
	if _, err := FindArtifactByNameOrPath(database, 2, "runs/model.bin"); err != nil {
		t.Fatalf("FindArtifactByNameOrPath by path: %v", err)
	}
}

func TestFindArtifactByBasename(t *testing.T) {
	database := setupTestDB(t)

	art := Artifact{
		JobID:      3,
		Name:       "",
		Path:       "/workspace/llm-performance-models/data/exports/calibration_53bbfeb29e50.jsonl",
		StoredPath: "3/workspace/llm-performance-models/data/exports/calibration_53bbfeb29e50.jsonl",
		SizeBytes:  27049,
		SHA256:     "dc63abc",
	}
	if err := UpsertArtifact(database, art); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	// Basename match
	found, err := FindArtifactByNameOrPath(database, 3, "calibration_53bbfeb29e50.jsonl")
	if err != nil {
		t.Fatalf("FindArtifactByNameOrPath by basename: %v", err)
	}
	if found.Path != art.Path {
		t.Fatalf("expected path %q, got %q", art.Path, found.Path)
	}

	// Suffix match
	found, err = FindArtifactByNameOrPath(database, 3, "data/exports/calibration_53bbfeb29e50.jsonl")
	if err != nil {
		t.Fatalf("FindArtifactByNameOrPath by suffix: %v", err)
	}
	if found.Path != art.Path {
		t.Fatalf("expected path %q, got %q", art.Path, found.Path)
	}

	// Exact full path still works
	found, err = FindArtifactByNameOrPath(database, 3, art.Path)
	if err != nil {
		t.Fatalf("FindArtifactByNameOrPath by full path: %v", err)
	}
	if found.Path != art.Path {
		t.Fatalf("expected path %q, got %q", art.Path, found.Path)
	}

	// Non-matching token
	_, err = FindArtifactByNameOrPath(database, 3, "nonexistent.jsonl")
	if err == nil {
		t.Fatal("expected error for non-matching token, got nil")
	}
}

func TestArtifactsPreferLatestRun(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := RecordQueued(database, "host1", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	run1 := *job.LatestRunID
	if err := UpsertArtifact(database, Artifact{
		JobID:      jobID,
		JobRunID:   &run1,
		Name:       "model",
		Path:       "output/model.bin",
		StoredPath: "run1/model.bin",
		SizeBytes:  10,
		SHA256:     "aaa",
	}); err != nil {
		t.Fatalf("UpsertArtifact(run1): %v", err)
	}

	if err := RequeueByID(database, jobID); err != nil {
		t.Fatalf("RequeueByID: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning second run: %v", err)
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID second run: %v", err)
	}
	run2 := *job.LatestRunID
	if run2 == run1 {
		t.Fatal("expected second run to get a new run ID")
	}
	if err := UpsertArtifact(database, Artifact{
		JobID:      jobID,
		JobRunID:   &run2,
		Name:       "model",
		Path:       "output/model.bin",
		StoredPath: "run2/model.bin",
		SizeBytes:  11,
		SHA256:     "bbb",
	}); err != nil {
		t.Fatalf("UpsertArtifact(run2): %v", err)
	}

	arts, err := ListArtifactsByJob(database, jobID)
	if err != nil {
		t.Fatalf("ListArtifactsByJob: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 latest-run artifact, got %d", len(arts))
	}
	if arts[0].StoredPath != "run2/model.bin" {
		t.Fatalf("stored_path = %q, want %q", arts[0].StoredPath, "run2/model.bin")
	}
	if arts[0].JobRunID == nil || *arts[0].JobRunID != run2 {
		t.Fatalf("job_run_id = %v, want %d", arts[0].JobRunID, run2)
	}

	art, err := FindArtifactByNameOrPath(database, jobID, "model")
	if err != nil {
		t.Fatalf("FindArtifactByNameOrPath: %v", err)
	}
	if art.StoredPath != "run2/model.bin" {
		t.Fatalf("latest artifact stored_path = %q, want %q", art.StoredPath, "run2/model.bin")
	}
}

// Regression: LIKE metacharacters in a token must match literally — a token
// containing '_' or '%' must not act as a wildcard over other rows.
func TestFindArtifactByNameOrPath_EscapesLikeMetacharacters(t *testing.T) {
	database := setupTestDB(t)
	jobID, err := RecordQueued(database, "host-alpha", "/tmp/p", "cmd", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"output/run_1.json", "output/runX1.json"} {
		if err := UpsertArtifact(database, Artifact{JobID: jobID, Path: path, StoredPath: path}); err != nil {
			t.Fatal(err)
		}
	}

	art, err := FindArtifactByNameOrPath(database, jobID, "run_1.json")
	if err != nil {
		t.Fatalf("FindArtifactByNameOrPath: %v", err)
	}
	if art.Path != "output/run_1.json" {
		t.Fatalf("path = %q, want the literal underscore match", art.Path)
	}

	// A token that matches only via wildcard interpretation must miss.
	if _, err := FindArtifactByNameOrPath(database, jobID, "run%.json"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wildcard token error = %v, want sql.ErrNoRows", err)
	}
}
