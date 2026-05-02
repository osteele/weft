package db

import (
	"errors"
	"testing"
	"time"
)

func TestCreatePlacementIntent(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	intent, err := CreatePlacementIntent(database, 100, "auto_relaunch")
	if err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}
	if intent.State != PlacementIntentStateOpen {
		t.Fatalf("state = %q, want open", intent.State)
	}
	if intent.Operation != "auto_relaunch" {
		t.Fatalf("operation = %q", intent.Operation)
	}
}

func TestCreatePlacementIntent_RejectsSecondOpen(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	if _, err := CreatePlacementIntent(database, 100, "first"); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := CreatePlacementIntent(database, 100, "second")
	if !errors.Is(err, ErrPlacementIntentAlreadyOpen) {
		t.Fatalf("err = %v, want ErrPlacementIntentAlreadyOpen", err)
	}
}

func TestResolvePlacementIntent(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	intent, err := CreatePlacementIntent(database, 100, "test")
	if err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}
	if err := ResolvePlacementIntent(database, intent.ID, PlacementIntentStateConfirmed, "ok"); err != nil {
		t.Fatalf("ResolvePlacementIntent: %v", err)
	}
	got, _ := GetPlacementIntent(database, intent.ID)
	if got.State != PlacementIntentStateConfirmed {
		t.Fatalf("state = %q, want confirmed", got.State)
	}
}

func TestJobIDsWithOpenPlacementIntents(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "j1", "/tmp", StatusQueued)
	insertTestJob(t, database, 200, "j2", "/tmp", StatusQueued)

	if _, err := CreatePlacementIntent(database, 100, "x"); err != nil {
		t.Fatalf("create 100: %v", err)
	}
	intent200, err := CreatePlacementIntent(database, 200, "x")
	if err != nil {
		t.Fatalf("create 200: %v", err)
	}
	if err := ResolvePlacementIntent(database, intent200.ID, PlacementIntentStateConfirmed, "done"); err != nil {
		t.Fatalf("resolve 200: %v", err)
	}

	got, err := JobIDsWithOpenPlacementIntents(database)
	if err != nil {
		t.Fatalf("JobIDsWithOpenPlacementIntents: %v", err)
	}
	if _, ok := got[100]; !ok {
		t.Errorf("job 100 missing")
	}
	if _, ok := got[200]; ok {
		t.Errorf("job 200 should not be in open set after resolve")
	}
}

func TestPrunePlacementIntents(t *testing.T) {
	t.Run("prunes stale intent on job whose only launch is terminal", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "j", "/tmp", StatusQueued)
		// Create a terminal launch and link the job's attempt to it.
		launchID, err := CreateLaunch(database, &Launch{Status: "failed", Provider: "vastai"})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if _, err := database.Exec(
			`UPDATE job_attempts SET launch_id = ? WHERE job_id = ?`,
			launchID, 100,
		); err != nil {
			t.Fatalf("link attempt: %v", err)
		}
		intent, err := CreatePlacementIntent(database, 100, "auto_relaunch")
		if err != nil {
			t.Fatalf("CreatePlacementIntent: %v", err)
		}
		// Backdate the intent past the protection window.
		if _, err := database.Exec(
			`UPDATE placement_intents SET created_at = ? WHERE id = ?`,
			time.Now().Add(-time.Hour).Unix(), intent.ID,
		); err != nil {
			t.Fatalf("backdate: %v", err)
		}

		pruned, err := PrunePlacementIntents(database, time.Minute)
		if err != nil {
			t.Fatalf("PrunePlacementIntents: %v", err)
		}
		if len(pruned) != 1 || pruned[0].JobID != 100 {
			t.Fatalf("pruned = %+v, want one for job 100", pruned)
		}
		open, _ := JobIDsWithOpenPlacementIntents(database)
		if _, ok := open[100]; ok {
			t.Errorf("job 100's intent should have been canceled")
		}
	})

	t.Run("protection window shields fresh intent even when no launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 200, "j", "/tmp", StatusQueued)
		if _, err := CreatePlacementIntent(database, 200, "auto_relaunch"); err != nil {
			t.Fatalf("CreatePlacementIntent: %v", err)
		}

		pruned, err := PrunePlacementIntents(database, 10*time.Minute)
		if err != nil {
			t.Fatalf("PrunePlacementIntents: %v", err)
		}
		if len(pruned) != 0 {
			t.Fatalf("fresh intent should be protected, got %+v", pruned)
		}
	})

	t.Run("keeps intent backed by non-terminal launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 300, "j", "/tmp", StatusQueued)
		launchID, err := CreateLaunch(database, &Launch{Status: "running", Provider: "vastai"})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if _, err := database.Exec(
			`UPDATE job_attempts SET launch_id = ? WHERE job_id = ?`,
			launchID, 300,
		); err != nil {
			t.Fatalf("link attempt: %v", err)
		}
		intent, err := CreatePlacementIntent(database, 300, "auto_relaunch")
		if err != nil {
			t.Fatalf("CreatePlacementIntent: %v", err)
		}
		// Backdate so age alone wouldn't protect it.
		if _, err := database.Exec(
			`UPDATE placement_intents SET created_at = ? WHERE id = ?`,
			time.Now().Add(-time.Hour).Unix(), intent.ID,
		); err != nil {
			t.Fatalf("backdate: %v", err)
		}

		pruned, err := PrunePlacementIntents(database, time.Minute)
		if err != nil {
			t.Fatalf("PrunePlacementIntents: %v", err)
		}
		if len(pruned) != 0 {
			t.Fatalf("intent backed by running launch should not be pruned, got %+v", pruned)
		}
	})
}

func TestCancelStalePlacementIntents(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "j1", "/tmp", StatusQueued)
	insertTestJob(t, database, 200, "j2", "/tmp", StatusQueued)

	intent100, err := CreatePlacementIntent(database, 100, "stale")
	if err != nil {
		t.Fatalf("create 100: %v", err)
	}
	// Backdate the 100 intent.
	if _, err := database.Exec(
		`UPDATE placement_intents SET created_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour).Unix(), intent100.ID,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := CreatePlacementIntent(database, 200, "fresh"); err != nil {
		t.Fatalf("create 200: %v", err)
	}

	canceled, err := CancelStalePlacementIntents(database, time.Hour)
	if err != nil {
		t.Fatalf("CancelStalePlacementIntents: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1", canceled)
	}
	got, _ := JobIDsWithOpenPlacementIntents(database)
	if _, ok := got[100]; ok {
		t.Errorf("job 100 should have been canceled")
	}
	if _, ok := got[200]; !ok {
		t.Errorf("job 200 should still be open")
	}
}
