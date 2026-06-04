package syncorch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestImportCloudTimeseriesFileUsesExplicitRunID(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "host1", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	firstRunID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}

	if err := db.RequeueByID(database, jobID); err != nil {
		t.Fatalf("RequeueByID: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning second run: %v", err)
	}
	secondRunID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID second: %v", err)
	}

	path := filepath.Join(t.TempDir(), "timeseries.jsonl")
	if err := os.WriteFile(path, []byte(`{"ts":5000,"cpu_pct":10}
`), 0o644); err != nil {
		t.Fatalf("write timeseries: %v", err)
	}

	if err := importCloudTimeseriesFile(database, jobID, firstRunID, path, "single"); err != nil {
		t.Fatalf("importCloudTimeseriesFile: %v", err)
	}

	firstSamples, err := db.GetTimeseriesByRun(database, firstRunID)
	if err != nil {
		t.Fatalf("GetTimeseriesByRun first: %v", err)
	}
	if len(firstSamples) != 1 || firstSamples[0].Ts != 5000 {
		t.Fatalf("first run samples = %+v, want ts=5000", firstSamples)
	}
	secondSamples, err := db.GetTimeseriesByRun(database, secondRunID)
	if err != nil {
		t.Fatalf("GetTimeseriesByRun second: %v", err)
	}
	if len(secondSamples) != 0 {
		t.Fatalf("second run samples = %+v, want none", secondSamples)
	}
}
