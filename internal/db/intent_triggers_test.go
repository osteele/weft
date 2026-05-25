package db

import (
	"testing"
)

// These tests cover the DB triggers added in
// 00004_intent_auto_resolution.sql + 00005_intent_auto_resolution_launch_status.sql,
// which make placement-intent leaks structurally impossible: any
// job_attempts insert/update that lands a job on a live launch resolves
// the open intent in the same transaction.

func TestTrigger_AutoConfirmMoveIntentOnAttemptInsert_LiveLaunch(t *testing.T) {
	database := setupTestDB(t)

	live, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 9001, "python x.py", "/tmp", StatusQueued)
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      9001,
		TargetKind: MoveTargetNew,
		// target_launch_id intentionally left NULL — the wj2206 shape.
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := CreateAttempt(database, 9001, "", &live, StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != MoveIntentStateConfirmed {
		t.Fatalf("state = %q, want confirmed", got.State)
	}
	if got.TargetLaunchID == nil || *got.TargetLaunchID != live {
		t.Fatalf("target_launch_id = %v, want %d (auto-populated from attempt)", got.TargetLaunchID, live)
	}
}

func TestTrigger_NoAutoConfirm_OnFailedLaunch(t *testing.T) {
	// Inserting an attempt against a failed launch is the precondition for
	// ResetLaunchJobs / HandleMoveTargetFailedBeforeStart. The intent must
	// remain open so those flows can restore the source.
	database := setupTestDB(t)

	failed, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 9002, "python x.py", "/tmp", StatusQueued)
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          9002,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &failed,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := CreateAttempt(database, 9002, "", &failed, StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != MoveIntentStateOpen {
		t.Fatalf("state = %q, want open (failed launch must not trigger confirm)", got.State)
	}
}

func TestTrigger_AutoConfirmOnLaunchBecomingLive(t *testing.T) {
	// An attempt can be created against a launching/planned instance before
	// the launch reaches a live status. The intent must auto-confirm when
	// the launch transitions to running/grace.
	database := setupTestDB(t)

	launching, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 9003, "python x.py", "/tmp", StatusQueued)
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          9003,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &launching,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := CreateAttempt(database, 9003, "", &launching, StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// Intent stays open while launch is launching (status not yet live by
	// the trigger's gating set — launching is included in the on-attempt
	// triggers, so this confirms immediately. The launch-status trigger
	// covers the late-running case.)
	if _, err := database.Exec(`UPDATE launches SET status = ? WHERE id = ?`, LaunchStatusRunning, launching); err != nil {
		t.Fatalf("update launch status: %v", err)
	}

	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != MoveIntentStateConfirmed {
		t.Fatalf("state = %q, want confirmed", got.State)
	}
}

func TestTrigger_AutoObsoleteMoveIntentOnSourceAttemptEnd(t *testing.T) {
	// spec/job-move.allium ObsoleteMoveOnSourceCompletion: when the source
	// attempt ends (any exit code), the open move is obsoleted.
	database := setupTestDB(t)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 9004, "python x.py", "/tmp", StatusQueued)
	sourceAttempt, err := CreateAttempt(database, 9004, "", &src, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:           9004,
		TargetKind:      MoveTargetNew,
		SourceLaunchID:  &src,
		SourceAttemptID: &sourceAttempt,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}

	if _, err := database.Exec(`UPDATE job_attempts SET end_time = 12345, status = ? WHERE id = ?`, StatusCompleted, sourceAttempt); err != nil {
		t.Fatalf("end source attempt: %v", err)
	}

	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != MoveIntentStateObsoleted {
		t.Fatalf("state = %q, want obsoleted", got.State)
	}
}

func TestTrigger_AutoResolveIntentsOnUserCancel(t *testing.T) {
	database := setupTestDB(t)
	insertTestJob(t, database, 9005, "python x.py", "/tmp", StatusQueued)
	moveIntent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      9005,
		TargetKind: MoveTargetNew,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	placeIntent, err := CreatePlacementIntent(database, 9005, "manual")
	if err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}

	if _, err := database.Exec(`UPDATE jobs SET requested_status = 'canceled' WHERE id = ?`, 9005); err != nil {
		t.Fatalf("set requested_status: %v", err)
	}

	gotMove, err := GetMoveIntent(database, moveIntent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotMove.State != MoveIntentStateObsoleted {
		t.Fatalf("move state = %q, want obsoleted", gotMove.State)
	}
	gotPlace, err := GetPlacementIntent(database, placeIntent.ID)
	if err != nil {
		t.Fatalf("GetPlacementIntent: %v", err)
	}
	if gotPlace.State != PlacementIntentStateCanceled {
		t.Fatalf("placement state = %q, want canceled", gotPlace.State)
	}
}

func TestTrigger_AutoConfirmPlacementIntent(t *testing.T) {
	database := setupTestDB(t)
	live, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 9006, "python x.py", "/tmp", StatusQueued)
	intent, err := CreatePlacementIntent(database, 9006, "test")
	if err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}
	if _, err := CreateAttempt(database, 9006, "", &live, StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	got, err := GetPlacementIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetPlacementIntent: %v", err)
	}
	if got.State != PlacementIntentStateConfirmed {
		t.Fatalf("state = %q, want confirmed", got.State)
	}
}
