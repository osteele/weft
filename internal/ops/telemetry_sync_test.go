package ops

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestSyncJobTimeseriesArchivesAfterCallerJobBecomesStale(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp", "work", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	staleJob, err := db.GetJobByID(database, jobID)
	if err != nil || staleJob.LatestRunID == nil {
		t.Fatalf("running job = %+v, %v", staleJob, err)
	}
	runID := *staleJob.LatestRunID
	if err := db.InsertTimeseries(database, jobID, []db.TimeseriesSample{{Ts: 10, CPUPct: 25}}); err != nil {
		t.Fatal(err)
	}
	if err := db.RefreshTimeseriesSummaryFromRows(database, jobID, runID); err != nil {
		t.Fatal(err)
	}
	if err := RecordJobCompletion(database, jobID, 0, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	previous := runTimeseriesSyncSSH
	runTimeseriesSyncSSH = func(string, string, time.Duration) (string, string, error) {
		return "{\"ts\":10,\"cpu_pct\":25}\n", "", nil
	}
	t.Cleanup(func() { runTimeseriesSyncSSH = previous })

	if err := syncJobTimeseries(database, staleJob, time.Second); err != nil {
		t.Fatalf("sync terminal timeseries: %v", err)
	}
	if rows, err := db.GetTimeseriesByRun(database, runID); err != nil || len(rows) != 0 {
		t.Fatalf("timeseries rows after archival = %v, %v", rows, err)
	}
	if object, err := db.GetRawTelemetryObject(database, runID, db.TimeseriesRawKind); err != nil || object == nil {
		t.Fatalf("timeseries archive = %+v, %v", object, err)
	}
}

func TestSyncJobTelemetryArchivesAfterCallerJobBecomesStale(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp", "work", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	staleJob, err := db.GetJobByID(database, jobID)
	if err != nil || staleJob.LatestRunID == nil {
		t.Fatalf("running job = %+v, %v", staleJob, err)
	}
	runID := *staleJob.LatestRunID
	if err := db.InsertTelemetrySamples(database, jobID, []db.TelemetrySample{{Ts: 10, ElapsedS: 1, ProcRSSKB: 42}}); err != nil {
		t.Fatal(err)
	}
	if err := db.RefreshJobTelemetrySummary(database, jobID); err != nil {
		t.Fatal(err)
	}
	if err := RecordJobCompletion(database, jobID, 0, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	previous := runTelemetrySyncSSH
	runTelemetrySyncSSH = func(string, string, time.Duration) (string, string, error) {
		return "{\"ts\":10,\"elapsed_s\":1,\"proc_rss_kb\":42}\n", "", nil
	}
	t.Cleanup(func() { runTelemetrySyncSSH = previous })

	if err := syncJobTelemetry(database, staleJob, time.Second); err != nil {
		t.Fatalf("sync terminal telemetry: %v", err)
	}
	if rows, err := db.GetTelemetryByRun(database, runID); err != nil || len(rows) != 0 {
		t.Fatalf("telemetry rows after archival = %v, %v", rows, err)
	}
	if object, err := db.GetRawTelemetryObject(database, runID, db.TelemetryRawKind); err != nil || object == nil {
		t.Fatalf("telemetry archive = %+v, %v", object, err)
	}
}
