package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestMarkReleasedInstanceFailed_ResetsUnresolvedJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusGrace,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo test", "test", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, db.StatusRunning, jobID); err != nil {
		t.Fatalf("set job running: %v", err)
	}

	if err := markReleasedInstanceFailed(database, instanceID); err != nil {
		t.Fatalf("markReleasedInstanceFailed: %v", err)
	}

	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.CloudInstanceStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.CloudInstanceStatusFailed)
	}
	if ci.TerminationReason != db.TerminationReasonJobFailure {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, db.TerminationReasonJobFailure)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusQueued)
	}
	if job.CloudInstanceID != nil {
		t.Fatalf("job cloud_instance_id = %v, want nil", *job.CloudInstanceID)
	}
	if job.Host != "" {
		t.Fatalf("job host = %q, want empty", job.Host)
	}

	outcomes, err := db.GetAttemptOutcomesByInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get outcomes: %v", err)
	}
	if outcomes[jobID] != db.AttemptOutcomeOrphaned {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[jobID], db.AttemptOutcomeOrphaned)
	}
}
