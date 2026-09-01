package db

import "testing"

func TestTelemetryArchiveCandidatesExcludeActiveAttempts(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "host-alpha", "/tmp", "work", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	if err := InsertTelemetrySamples(database, jobID, []TelemetrySample{{Ts: 10}}); err != nil {
		t.Fatal(err)
	}
	candidates, err := ListTelemetryArchiveCandidates(database, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("active candidates = %+v", candidates)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordCompletionByID(database, jobID, 0, job.StartTime+1); err != nil {
		t.Fatal(err)
	}
	candidates, err = ListTelemetryArchiveCandidates(database, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RichSamples != 1 {
		t.Fatalf("terminal candidates = %+v", candidates)
	}
}
