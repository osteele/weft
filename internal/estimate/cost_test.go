package estimate

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestRentalHistoryCostSummaryTotalsDistinctExclusiveLaunches(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "cost history")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	const (
		launchedAt = int64(10_000)
		endedAt    = int64(13_600)
	)
	launchIDs := make([]int64, 0, 2)
	for range 2 {
		launchID, err := db.CreateLaunch(database, &db.Launch{
			Status:   db.LaunchStatusRunning,
			Provider: "vastai",
			GPUSpec:  "L40",
		})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if _, err := database.Exec(`
			UPDATE launches
			   SET status = ?, launched_at = ?, ended_at = ?,
			       cost_per_hour_cents = ?, termination_reason = ?
			 WHERE id = ?`,
			db.LaunchStatusCompleted, launchedAt, endedAt, 50,
			db.TerminationReasonCompleted, launchID,
		); err != nil {
			t.Fatalf("finalize launch: %v", err)
		}
		launchIDs = append(launchIDs, launchID)
	}

	if err := db.SetJobLaunchID(database, jobID, launchIDs[0]); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	exitCode := 1
	if err := db.CloseAttempt(database, jobID, db.StatusFailed, &exitCode, endedAt); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", &launchIDs[1], db.StatusFailed); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	info, ok := RentalHistoryCostSummary(database, &db.Job{ID: jobID}, time.Unix(endedAt, 0))
	if !ok {
		t.Fatal("RentalHistoryCostSummary returned no history")
	}
	if info.Launches != 2 || info.Cost != 1.0 || info.Provisional {
		t.Fatalf("RentalHistoryCostSummary = %+v, want 2 launches costing $1.00", info)
	}
}
