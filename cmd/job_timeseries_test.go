package cmd

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestLoadRawTimeseriesFallsBackToDBRowsWhenRetainedR2Unavailable(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "cloud", "/tmp/project", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LatestRunID == nil {
		t.Fatalf("latest run id missing")
	}
	runID := *job.LatestRunID

	if err := db.UpsertTimeseriesRawObject(database, db.TimeseriesRawObject{
		JobID:     jobID,
		AttemptID: runID,
		Kind:      db.TimeseriesRawKind,
		R2Key:     "jobs/1/runs/1/telemetry/timeseries.jsonl",
		ETag:      "stale-etag",
	}); err != nil {
		t.Fatalf("UpsertTimeseriesRawObject: %v", err)
	}
	if err := db.InsertTimeseriesForRun(database, jobID, runID, []db.TimeseriesSample{{
		Ts:     123,
		CPUPct: 45,
		RSSKB:  678,
		Tenant: "single",
	}}); err != nil {
		t.Fatalf("InsertTimeseriesForRun: %v", err)
	}

	oldFetch := fetchR2TimeseriesObjectFunc
	fetchR2TimeseriesObjectFunc = func(string) ([]byte, error) {
		return nil, errors.New("r2 unavailable")
	}
	t.Cleanup(func() { fetchR2TimeseriesObjectFunc = oldFetch })

	data, source, err := loadRawTimeseries(database, jobID, runID)
	if err != nil {
		t.Fatalf("loadRawTimeseries: %v", err)
	}
	if source != "db" {
		t.Fatalf("source = %q, want db", source)
	}
	var sample db.TimeseriesSample
	if err := json.Unmarshal(data, &sample); err != nil {
		t.Fatalf("decode fallback JSONL: %v", err)
	}
	if sample.Ts != 123 || sample.CPUPct != 45 || sample.RSSKB != 678 || sample.Tenant != "single" {
		t.Fatalf("sample = %+v", sample)
	}
}
