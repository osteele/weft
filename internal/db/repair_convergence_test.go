package db

import (
	"testing"
	"time"
)

func assertRepairSweepConverges(t *testing.T, name string, setup func(t *testing.T) func(t *testing.T) int) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Helper()
		pass := setup(t)
		first := pass(t)
		if first == 0 {
			t.Fatalf("first repair pass touched 0 rows, want a load-bearing fixture")
		}
		second := pass(t)
		if second != 0 {
			t.Fatalf("second repair pass touched %d rows, want 0", second)
		}
	})
}

func TestRepairSweepConvergenceContracts(t *testing.T) {
	assertRepairSweepConverges(t, "terminal launch reset", func(t *testing.T) func(t *testing.T) int {
		database := setupTestDB(t)
		launchID, err := CreateLaunch(database, &Launch{
			Status:            LaunchStatusFailed,
			Provider:          "vastai",
			GPUSpec:           "RTX 4090",
			TerminationReason: TerminationReasonInfraFailure,
		})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		insertTestJob(t, database, 1, "echo test", "/tmp", StatusRunning, withLaunch(launchID))

		return func(t *testing.T) int {
			resetMap, err := ResetJobsOnTerminalLaunches(database)
			if err != nil {
				t.Fatalf("ResetJobsOnTerminalLaunches: %v", err)
			}
			return len(resetMap)
		}
	})

	assertRepairSweepConverges(t, "stale move intent prune", func(t *testing.T) func(t *testing.T) int {
		database := setupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:      100,
			TargetKind: MoveTargetNew,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if _, err := database.Exec(
			`UPDATE move_intents SET created_at = ? WHERE id = ?`,
			time.Now().Add(-MoveIntentRetryPruneAgeLimit-time.Minute).Unix(), intent.ID,
		); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		return func(t *testing.T) int {
			pruned, err := PruneMoveIntents(database, 5*time.Minute)
			if err != nil {
				t.Fatalf("PruneMoveIntents: %v", err)
			}
			return len(pruned)
		}
	})
}
