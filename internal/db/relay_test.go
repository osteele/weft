package db

import "testing"

func TestRecordQueuedWithGPUAndIDUpsertsExistingJob(t *testing.T) {
	database := SetupTestDB(t)

	const jobID int64 = 42
	if err := RecordQueuedWithGPUAndID(database, jobID, "host-a", "/tmp/project-a", "python first.py", "first job", "0"); err != nil {
		t.Fatalf("initial record: %v", err)
	}
	if err := RecordQueuedWithGPUAndID(database, jobID, "host-b", "/tmp/project-b", "python second.py", "updated job", "1"); err != nil {
		t.Fatalf("upsert record: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil {
		t.Fatalf("job %d not found", jobID)
	}
	if job.Host != "host-b" {
		t.Fatalf("host = %q, want %q", job.Host, "host-b")
	}
	if job.WorkingDir != "/tmp/project-b" {
		t.Fatalf("working dir = %q, want %q", job.WorkingDir, "/tmp/project-b")
	}
	if job.Command != "python second.py" {
		t.Fatalf("command = %q, want %q", job.Command, "python second.py")
	}
	if job.Description != "updated job" {
		t.Fatalf("description = %q, want %q", job.Description, "updated job")
	}
	if job.GPU != "1" {
		t.Fatalf("gpu = %q, want %q", job.GPU, "1")
	}
}

func TestProcessedRelayRequests(t *testing.T) {
	database := SetupTestDB(t)

	processed, err := IsRelayRequestProcessed(database, "req-1")
	if err != nil {
		t.Fatalf("IsRelayRequestProcessed before insert: %v", err)
	}
	if processed {
		t.Fatalf("request unexpectedly marked processed before insert")
	}

	if err := RecordProcessedRelayRequest(database, "req-1", "submit_job", 101); err != nil {
		t.Fatalf("RecordProcessedRelayRequest: %v", err)
	}

	processed, err = IsRelayRequestProcessed(database, "req-1")
	if err != nil {
		t.Fatalf("IsRelayRequestProcessed after insert: %v", err)
	}
	if !processed {
		t.Fatalf("request was not marked processed")
	}

	if err := RecordProcessedRelayRequest(database, "req-1", "submit_job", 101); err != nil {
		t.Fatalf("RecordProcessedRelayRequest duplicate: %v", err)
	}
}
