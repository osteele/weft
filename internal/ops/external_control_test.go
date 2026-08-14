package ops

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestLocalOperationsRejectExternalJobsBeforeSSHOrMutation(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42", ExternalTaskID: "task-a",
		RawStatus: "RUNNING", NormalizedStatus: db.StatusRunning, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("get external job: %v", err)
	}
	sshCalls := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		sshCalls++
		return "", "", 0
	})

	operations := []struct {
		name string
		run  func() error
	}{
		{name: "kill", run: func() error { _, err := KillJob(database, job, DefaultOptions()); return err }},
		{name: "cancel queued", run: func() error { _, err := CancelQueuedJob(database, job, DefaultOptions()); return err }},
		{name: "draft", run: func() error { _, err := DraftJob(database, job, DefaultOptions()); return err }},
		{name: "request status", run: func() error { _, err := RequestStatus(database, job, db.StatusDraft, TimeoutFast); return err }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); err == nil || !strings.Contains(err.Error(), "SkyPilot") {
				t.Fatalf("operation error = %v, want SkyPilot refusal", err)
			}
		})
	}
	if sshCalls != 0 {
		t.Fatalf("SSH calls = %d, want none", sshCalls)
	}
	refreshed, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("reload external job: %v", err)
	}
	var requested sql.NullString
	if err := database.QueryRow(`SELECT requested_status FROM jobs WHERE id = ?`, binding.JobID).Scan(&requested); err != nil {
		t.Fatalf("read requested status: %v", err)
	}
	if refreshed.Status != db.StatusRunning || requested.Valid {
		t.Fatalf("operations mutated external job: status=%q requested=%v", refreshed.Status, requested)
	}
}
