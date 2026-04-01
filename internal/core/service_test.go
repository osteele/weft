package core

import (
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

func TestKillJobUsesEffectiveStatusForHostlessRunning(t *testing.T) {
	// In the new model, a hostless job has no attempt and shows as "queued"
	// from requested_status. KillJob should still cancel it.
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp", "sleep 100", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	service := NewServiceWithDB(database)
	result, err := service.KillJob(jobID, ops.TimeoutFast)
	if err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	if !result.Outcome.Success {
		t.Fatalf("expected success outcome, got %+v", result.Outcome)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.EffectiveStatus() != db.StatusCanceled {
		t.Fatalf("effective status = %q, want %q", job.EffectiveStatus(), db.StatusCanceled)
	}
}
