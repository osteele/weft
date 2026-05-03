package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestCancelDurablyVisibleRequiresCanceledStatus(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "echo hi", "cancel")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	verified, status, err := cancelDurablyVisible(database, jobID)
	if err != nil {
		t.Fatalf("cancelDurablyVisible queued: %v", err)
	}
	if verified {
		t.Fatalf("verified queued job as canceled; status=%s", status)
	}

	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusCanceled); err != nil {
		t.Fatalf("UpdateStatusAndLastSynced: %v", err)
	}
	if err := db.SetRequestedStatus(database, jobID, db.StatusCanceled); err != nil {
		t.Fatalf("SetRequestedStatus: %v", err)
	}
	verified, status, err = cancelDurablyVisible(database, jobID)
	if err != nil {
		t.Fatalf("cancelDurablyVisible canceled: %v", err)
	}
	if !verified || status != db.StatusCanceled {
		t.Fatalf("verified=%v status=%s, want canceled", verified, status)
	}
}
