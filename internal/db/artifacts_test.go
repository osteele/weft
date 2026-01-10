package db

import "testing"

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
