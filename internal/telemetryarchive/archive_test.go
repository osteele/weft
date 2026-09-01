package telemetryarchive

import (
	"testing"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/db"
)

func TestFinalizeRichRecordsObjectAndPrunesRows(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp", "work", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job.LatestRunID == nil {
		t.Fatalf("job run: %+v %v", job, err)
	}
	samples := []db.TelemetrySample{{Ts: 10, ElapsedS: 1, ProcRSSKB: 42}}
	if err := db.InsertTelemetrySamples(database, jobID, samples); err != nil {
		t.Fatal(err)
	}
	raw := []byte("{\"ts\":10,\"elapsed_s\":1,\"proc_rss_kb\":42}\n")
	rollup := db.BuildRichTelemetryRollup(jobID, *job.LatestRunID, samples, 1, nil)
	if err := FinalizeRich(database, jobID, *job.LatestRunID, raw, rollup, RemoteCopy{}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if rows, err := db.GetTelemetryByRun(database, *job.LatestRunID); err != nil || len(rows) != 0 {
		t.Fatalf("raw rows = %v, %v", rows, err)
	}
	gotRollup, err := db.GetRichTelemetryRollup(database, *job.LatestRunID)
	if err != nil || gotRollup == nil || gotRollup.SampleCount != 1 {
		t.Fatalf("rollup = %+v, %v", gotRollup, err)
	}
	obj, err := db.GetRawTelemetryObject(database, *job.LatestRunID, db.TelemetryRawKind)
	if err != nil || obj == nil {
		t.Fatalf("object = %+v, %v", obj, err)
	}
	if _, err := artifacts.ReadSystemBlob(obj.StoredPath, obj.SizeBytes, obj.SHA256); err != nil {
		t.Fatalf("read archived bytes: %v", err)
	}
}

func TestFinalizeRichRejectsMalformedJSONLBeforePruning(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp", "work", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job.LatestRunID == nil {
		t.Fatalf("job run: %+v %v", job, err)
	}
	samples := []db.TelemetrySample{{Ts: 10, ProcRSSKB: 42}}
	if err := db.InsertTelemetrySamples(database, jobID, samples); err != nil {
		t.Fatal(err)
	}
	rollup := db.BuildRichTelemetryRollup(jobID, *job.LatestRunID, samples, 1, nil)
	if err := FinalizeRich(database, jobID, *job.LatestRunID, []byte("{not-json}\n"), rollup, RemoteCopy{}); err == nil {
		t.Fatal("malformed archive succeeded")
	}
	rows, err := db.GetTelemetryByRun(database, *job.LatestRunID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("raw rows after rejection = %v, %v", rows, err)
	}
}
