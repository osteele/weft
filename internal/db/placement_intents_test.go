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
