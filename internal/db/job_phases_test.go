package db

import "testing"

func TestJobPhaseTimingsRoundTripsHFPrewarmMetrics(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "", t.TempDir(), "true", "phase metrics")
	if err != nil {
		t.Fatal(err)
	}
	bytes := int64(8_000_000)
	durationMS := int64(4000)
	if err := UpsertJobPhaseTimings(database, &JobPhaseTimings{
		JobID: jobID, HFPrewarmDownloadedBytes: &bytes, HFPrewarmDurationMS: &durationMS,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := GetJobPhaseTimings(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.HFPrewarmDownloadedBytes == nil || *got.HFPrewarmDownloadedBytes != bytes {
		t.Fatalf("HF prewarm bytes = %+v, want %d", got, bytes)
	}
	if got.HFPrewarmDurationMS == nil || *got.HFPrewarmDurationMS != durationMS {
		t.Fatalf("HF prewarm duration = %+v, want %d ms", got.HFPrewarmDurationMS, durationMS)
	}
}
