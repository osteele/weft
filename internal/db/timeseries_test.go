package db

import (
	"testing"
)

func TestInsertAndGetTimeseries(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	samples := []TimeseriesSample{
		{Ts: 1000, CPUPct: 25, RSSKB: 400000, GPUMiB: 6000, Tenant: "multi"},
		{Ts: 1015, CPUPct: 28, RSSKB: 500000, GPUMiB: 8000, Tenant: "multi"},
		{
			Ts:             1030,
			CPUPct:         30,
			RSSKB:          520000,
			GPUMiB:         8100,
			DiskFreeBytes:  1_500_000_000,
			DiskTotalBytes: 3_000_000_000,
			HostRSSKB:      1000000,
			HostMemTotalKB: 16000000,
			Tenant:         "single",
		},
	}

	if err := InsertTimeseries(database, 42, samples); err != nil {
		t.Fatalf("InsertTimeseries: %v", err)
	}

	// Read back
	got, err := GetTimeseries(database, 42)
	if err != nil {
		t.Fatalf("GetTimeseries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 samples, got %d", len(got))
	}

	// Verify first sample
	if got[0].Ts != 1000 || got[0].CPUPct != 25 || got[0].RSSKB != 400000 {
		t.Errorf("sample 0 mismatch: %+v", got[0])
	}
	if got[0].Tenant != "multi" {
		t.Errorf("expected tenant=multi, got %q", got[0].Tenant)
	}

	// Verify third sample with host metrics
	if got[2].HostRSSKB != 1000000 || got[2].HostMemTotalKB != 16000000 {
		t.Errorf("sample 2 host metrics mismatch: %+v", got[2])
	}
	if got[2].DiskFreeBytes != 1_500_000_000 || got[2].DiskTotalBytes != 3_000_000_000 {
		t.Errorf("sample 2 disk metrics mismatch: %+v", got[2])
	}
	if got[2].Tenant != "single" {
		t.Errorf("expected tenant=single, got %q", got[2].Tenant)
	}
}

func TestInsertTimeseries_Idempotent(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	samples := []TimeseriesSample{
		{Ts: 1000, CPUPct: 25, RSSKB: 400000},
		{Ts: 1015, CPUPct: 28, RSSKB: 500000},
	}

	// Insert twice
	if err := InsertTimeseries(database, 42, samples); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := InsertTimeseries(database, 42, samples); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	got, err := GetTimeseries(database, 42)
	if err != nil {
		t.Fatalf("GetTimeseries: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 samples after idempotent insert, got %d", len(got))
	}
}

func TestGetTimeseriesLastTS(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// No data yet
	ts, err := GetTimeseriesLastTS(database, 42)
	if err != nil {
		t.Fatalf("GetTimeseriesLastTS (empty): %v", err)
	}
	if ts != 0 {
		t.Errorf("expected 0 for empty, got %d", ts)
	}

	// Insert some data
	samples := []TimeseriesSample{
		{Ts: 1000, CPUPct: 25},
		{Ts: 1015, CPUPct: 28},
		{Ts: 1030, CPUPct: 30},
	}
	if err := InsertTimeseries(database, 42, samples); err != nil {
		t.Fatalf("InsertTimeseries: %v", err)
	}

	ts, err = GetTimeseriesLastTS(database, 42)
	if err != nil {
		t.Fatalf("GetTimeseriesLastTS: %v", err)
	}
	if ts != 1030 {
		t.Errorf("expected 1030, got %d", ts)
	}
}

func TestInsertTimeseries_Empty(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Should not error on empty slice
	if err := InsertTimeseries(database, 42, nil); err != nil {
		t.Fatalf("InsertTimeseries(nil): %v", err)
	}
	if err := InsertTimeseries(database, 42, []TimeseriesSample{}); err != nil {
		t.Fatalf("InsertTimeseries(empty): %v", err)
	}
}

func TestGetTimeseriesLastTS_UsesLatestRun(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	jobID, err := RecordQueued(database, "host1", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	if err := InsertTimeseries(database, jobID, []TimeseriesSample{{Ts: 5000, CPUPct: 10}}); err != nil {
		t.Fatalf("InsertTimeseries first run: %v", err)
	}

	if err := RequeueByID(database, jobID); err != nil {
		t.Fatalf("RequeueByID: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning second run: %v", err)
	}
	if err := InsertTimeseries(database, jobID, []TimeseriesSample{{Ts: 1000, CPUPct: 20}}); err != nil {
		t.Fatalf("InsertTimeseries second run: %v", err)
	}

	ts, err := GetTimeseriesLastTS(database, jobID)
	if err != nil {
		t.Fatalf("GetTimeseriesLastTS: %v", err)
	}
	if ts != 1000 {
		t.Fatalf("latest-run ts = %d, want %d", ts, 1000)
	}
}

func TestInsertTimeseriesForRunTargetsExplicitRun(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	jobID, err := RecordQueued(database, "host1", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	firstRunID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}

	if err := RequeueByID(database, jobID); err != nil {
		t.Fatalf("RequeueByID: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning second run: %v", err)
	}
	secondRunID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID second: %v", err)
	}

	if err := InsertTimeseriesForRun(database, jobID, firstRunID, []TimeseriesSample{{Ts: 5000, CPUPct: 10}}); err != nil {
		t.Fatalf("InsertTimeseriesForRun first run: %v", err)
	}
	if err := InsertTimeseriesForRun(database, jobID, secondRunID, []TimeseriesSample{{Ts: 1000, CPUPct: 20}}); err != nil {
		t.Fatalf("InsertTimeseriesForRun second run: %v", err)
	}

	firstSamples, err := GetTimeseriesByRun(database, firstRunID)
	if err != nil {
		t.Fatalf("GetTimeseriesByRun first: %v", err)
	}
	if len(firstSamples) != 1 || firstSamples[0].Ts != 5000 {
		t.Fatalf("first run samples = %+v, want ts=5000", firstSamples)
	}
	secondSamples, err := GetTimeseriesByRun(database, secondRunID)
	if err != nil {
		t.Fatalf("GetTimeseriesByRun second: %v", err)
	}
	if len(secondSamples) != 1 || secondSamples[0].Ts != 1000 {
		t.Fatalf("second run samples = %+v, want ts=1000", secondSamples)
	}
}

func TestGetTimeseriesLastTSForRunTargetsExplicitRun(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	jobID, err := RecordQueued(database, "host1", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	firstRunID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}
	if err := InsertTimeseriesForRun(database, jobID, firstRunID, []TimeseriesSample{{Ts: 5000, CPUPct: 10}}); err != nil {
		t.Fatalf("InsertTimeseriesForRun first run: %v", err)
	}

	if err := RequeueByID(database, jobID); err != nil {
		t.Fatalf("RequeueByID: %v", err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning second run: %v", err)
	}
	secondRunID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID second: %v", err)
	}
	if err := InsertTimeseriesForRun(database, jobID, secondRunID, []TimeseriesSample{{Ts: 1000, CPUPct: 20}}); err != nil {
		t.Fatalf("InsertTimeseriesForRun second run: %v", err)
	}

	firstTS, err := GetTimeseriesLastTSForRun(database, jobID, firstRunID)
	if err != nil {
		t.Fatalf("GetTimeseriesLastTSForRun first: %v", err)
	}
	if firstTS != 5000 {
		t.Fatalf("first-run ts = %d, want 5000", firstTS)
	}
	secondTS, err := GetTimeseriesLastTSForRun(database, jobID, secondRunID)
	if err != nil {
		t.Fatalf("GetTimeseriesLastTSForRun second: %v", err)
	}
	if secondTS != 1000 {
		t.Fatalf("second-run ts = %d, want 1000", secondTS)
	}
}
