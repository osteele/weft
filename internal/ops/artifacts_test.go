package ops

import (
	"database/sql"
	"testing"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	_ "modernc.org/sqlite"
)

func setupArtifactTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database := db.SetupTestDB(t)
	if err := dataloc.InitSchema(database); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestRecordJobOutputs_Success(t *testing.T) {
	database := setupArtifactTestDB(t)

	exitCode := 0
	job := &db.Job{
		ID:       42,
		Host:     "cool30",
		Status:   db.StatusCompleted,
		ExitCode: &exitCode,
		Outputs:  []string{"checkpoint:llama-ft-v1", "hf:my-org/fine-tuned-model"},
	}

	RecordJobOutputs(database, job)

	// Verify assets were recorded
	entries, err := dataloc.ListHostAssets(database, "cool30")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 recorded assets, got %d", len(entries))
	}

	// Verify checkpoint
	found := false
	for _, e := range entries {
		if e.Asset.Kind == dataloc.AssetCheckpoint && e.Asset.ID == "llama-ft-v1" {
			found = true
		}
	}
	if !found {
		t.Errorf("checkpoint:llama-ft-v1 not found in recorded assets: %v", entries)
	}
}

func TestRecordJobOutputs_FailedJob(t *testing.T) {
	database := setupArtifactTestDB(t)

	exitCode := 1
	job := &db.Job{
		ID:       43,
		Host:     "cool30",
		Status:   db.StatusCompleted,
		ExitCode: &exitCode,
		Outputs:  []string{"checkpoint:should-not-record"},
	}

	RecordJobOutputs(database, job)

	entries, err := dataloc.ListHostAssets(database, "cool30")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed job should not record outputs, got %d entries", len(entries))
	}
}

func TestRecordJobOutputs_NoOutputs(t *testing.T) {
	database := setupArtifactTestDB(t)

	exitCode := 0
	job := &db.Job{
		ID:       44,
		Host:     "cool30",
		Status:   db.StatusCompleted,
		ExitCode: &exitCode,
	}

	// Should not panic or error
	RecordJobOutputs(database, job)

	entries, err := dataloc.ListHostAssets(database, "cool30")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("job with no outputs should record nothing, got %d entries", len(entries))
	}
}

func TestRecordJobOutputs_NilJob(t *testing.T) {
	database := setupArtifactTestDB(t)
	// Should not panic
	RecordJobOutputs(database, nil)

	entries, err := dataloc.ListAllAssets(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("nil job should record nothing, got %d entries", len(entries))
	}
}

func TestRecordJobOutputs_KilledJob(t *testing.T) {
	database := setupArtifactTestDB(t)

	job := &db.Job{
		ID:      45,
		Host:    "cool30",
		Status:  db.StatusKilled,
		Outputs: []string{"checkpoint:should-not-record"},
	}

	RecordJobOutputs(database, job)

	entries, err := dataloc.ListHostAssets(database, "cool30")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("killed job should not record outputs, got %d entries", len(entries))
	}
}
