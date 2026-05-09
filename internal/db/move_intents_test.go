package db

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func mustCreateLaunch(t *testing.T, database *sql.DB) int64 {
	t.Helper()
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	return id
}

func TestCreateMoveIntent_Existing(t *testing.T) {
	database := SetupTestDB(t)
	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	target, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued, withLaunch(src))

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          100,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetExisting,
		TargetLaunchID: &target,
		TargetGPUName:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if intent.State != MoveIntentStateOpen {
		t.Fatalf("state = %q, want open", intent.State)
	}
	if intent.TargetLaunchID == nil || *intent.TargetLaunchID != target {
		t.Fatalf("target_launch_id = %v, want %d", intent.TargetLaunchID, target)
	}
	if intent.SourceLaunchID == nil || *intent.SourceLaunchID != src {
		t.Fatalf("source_launch_id = %v, want %d", intent.SourceLaunchID, src)
	}
}

func TestCreateMoveIntent_NewWithoutTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued, withLaunch(src))

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:               100,
		SourceLaunchID:      &src,
		TargetKind:          MoveTargetNew,
		TargetOfferProvider: "vastai",
		TargetOfferID:       "abc-123",
		TargetGPUName:       "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if intent.TargetLaunchID != nil {
		t.Fatalf("target_launch_id = %v, want nil for new", intent.TargetLaunchID)
	}
	if intent.TargetOfferProvider != "vastai" {
		t.Fatalf("offer provider = %q", intent.TargetOfferProvider)
	}
}

func TestCreateMoveIntent_RejectsExistingWithoutTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	_, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      100,
		TargetKind: MoveTargetExisting,
	})
	if err == nil {
		t.Fatal("expected error for existing target without launch id")
	}
}

func TestCreateMoveIntent_RejectsSecondOpenIntentForSameJob(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	if _, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	}); err != nil {
		t.Fatalf("first CreateMoveIntent: %v", err)
	}

	_, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if !errors.Is(err, ErrMoveIntentAlreadyOpen) {
		t.Fatalf("err = %v, want ErrMoveIntentAlreadyOpen", err)
	}
}

func TestCreateMoveIntent_AllowsNewIntentAfterPriorResolved(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	first, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := ResolveMoveIntent(database, first.ID, MoveIntentStateCanceled, "test"); err != nil {
		t.Fatalf("ResolveMoveIntent: %v", err)
	}

	_, err = CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("second after resolve: %v", err)
	}
}

func TestResolveMoveIntent_SetsResolutionAndTime(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateConfirmed, "moved"); err != nil {
		t.Fatalf("ResolveMoveIntent: %v", err)
	}

	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != MoveIntentStateConfirmed {
		t.Fatalf("state = %q, want confirmed", got.State)
	}
	if got.Resolution != "moved" {
		t.Fatalf("resolution = %q", got.Resolution)
	}
	if got.ResolvedAt == nil {
		t.Fatal("resolved_at not set")
	}
}

func TestResolveMoveIntent_NoOpOnAlreadyResolved(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateConfirmed, "first"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Second resolve to a different state must not change the record.
	if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateCanceled, "second"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	got, _ := GetMoveIntent(database, intent.ID)
	if got.State != MoveIntentStateConfirmed {
		t.Fatalf("state = %q, want confirmed (terminal state must not flip)", got.State)
	}
	if got.Resolution != "first" {
		t.Fatalf("resolution = %q, want %q", got.Resolution, "first")
	}
}

func TestUpdateMoveIntentTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetNew, TargetGPUName: "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	newLaunch := mustCreateLaunch(t, database)
	if err := UpdateMoveIntentTargetLaunch(database, intent.ID, newLaunch); err != nil {
		t.Fatalf("UpdateMoveIntentTargetLaunch: %v", err)
	}
	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.TargetLaunchID == nil || *got.TargetLaunchID != newLaunch {
		t.Fatalf("target_launch_id = %v, want %d", got.TargetLaunchID, newLaunch)
	}
}

func TestConfirmOpenMoveIntentsForTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
	insertTestJob(t, database, 200, "echo hi", "/tmp", StatusQueued)
	target := mustCreateLaunch(t, database)
	other := mustCreateLaunch(t, database)

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetNew, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent target: %v", err)
	}
	otherIntent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 200, TargetKind: MoveTargetNew, TargetLaunchID: &other,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent other: %v", err)
	}

	if err := ConfirmOpenMoveIntentsForTargetLaunch(database, target, "agent ready"); err != nil {
		t.Fatalf("ConfirmOpenMoveIntentsForTargetLaunch: %v", err)
	}
	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent target: %v", err)
	}
	if got.State != MoveIntentStateConfirmed || got.Resolution != "agent ready" {
		t.Fatalf("target intent = (%s, %q), want confirmed agent ready", got.State, got.Resolution)
	}
	gotOther, err := GetMoveIntent(database, otherIntent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent other: %v", err)
	}
	if gotOther.State != MoveIntentStateOpen {
		t.Fatalf("other intent state = %s, want open", gotOther.State)
	}
}

func TestJobIDsWithOpenMoveIntents(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "j1", "/tmp", StatusQueued)
	insertTestJob(t, database, 200, "j2", "/tmp", StatusQueued)
	insertTestJob(t, database, 300, "j3", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	if _, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	}); err != nil {
		t.Fatalf("create 100: %v", err)
	}
	open200, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 200, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("create 200: %v", err)
	}
	// Resolve 200 — should drop out of open set.
	if err := ResolveMoveIntent(database, open200.ID, MoveIntentStateConfirmed, "done"); err != nil {
		t.Fatalf("resolve 200: %v", err)
	}
	// Job 300 has no intent.

	got, err := JobIDsWithOpenMoveIntents(database)
	if err != nil {
		t.Fatalf("JobIDsWithOpenMoveIntents: %v", err)
	}
	if _, ok := got[100]; !ok {
		t.Errorf("job 100 missing from open set")
	}
	if _, ok := got[200]; ok {
		t.Errorf("job 200 should not be in open set after resolve")
	}
	if _, ok := got[300]; ok {
		t.Errorf("job 300 should not be in open set")
	}
}

func TestPruneMoveIntents(t *testing.T) {
	t.Run("stale new target without launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:      100,
			TargetKind: MoveTargetNew,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), intent.ID); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 1 || pruned[0].ID != intent.ID {
			t.Fatalf("pruned = %+v, want intent %d", pruned, intent.ID)
		}
		got, err := GetMoveIntent(database, intent.ID)
		if err != nil {
			t.Fatalf("GetMoveIntent: %v", err)
		}
		if got.State != MoveIntentStateCanceled || got.Resolution != MoveIntentResolutionStale {
			t.Fatalf("intent = (%s, %q), want canceled stale", got.State, got.Resolution)
		}
	})

	t.Run("keeps fresh unlaunched move", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:      100,
			TargetKind: MoveTargetNew,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 0 {
			t.Fatalf("pruned = %+v, want none", pruned)
		}
		got, err := GetMoveIntent(database, intent.ID)
		if err != nil {
			t.Fatalf("GetMoveIntent: %v", err)
		}
		if got.State != MoveIntentStateOpen {
			t.Fatalf("state = %s, want open", got.State)
		}
	})

	t.Run("keeps live target launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		target := mustCreateLaunch(t, database)
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:          100,
			TargetKind:     MoveTargetNew,
			TargetLaunchID: &target,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), intent.ID); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 0 {
			t.Fatalf("pruned = %+v, want none", pruned)
		}
	})

	t.Run("prunes terminal target launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		target, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:          100,
			TargetKind:     MoveTargetNew,
			TargetLaunchID: &target,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), intent.ID); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 1 || pruned[0].ID != intent.ID {
			t.Fatalf("pruned = %+v, want intent %d", pruned, intent.ID)
		}
	})
}

func TestAttachMoveIntentTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:       100,
		TargetKind:  MoveTargetNew,
		MaxAttempts: 4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	first := mustCreateLaunch(t, database)
	if err := AttachMoveIntentTargetLaunch(database, intent, first); err != nil {
		t.Fatalf("AttachMoveIntentTargetLaunch first: %v", err)
	}
	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent first: %v", err)
	}
	if got.TargetLaunchID == nil || *got.TargetLaunchID != first {
		t.Fatalf("target_launch_id = %v, want %d", got.TargetLaunchID, first)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt_count after first attach = %d, want 1", got.AttemptCount)
	}

	second := mustCreateLaunch(t, database)
	if err := AttachMoveIntentTargetLaunch(database, got, second); err != nil {
		t.Fatalf("AttachMoveIntentTargetLaunch second: %v", err)
	}
	got, err = GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent second: %v", err)
	}
	if got.TargetLaunchID == nil || *got.TargetLaunchID != second {
		t.Fatalf("target_launch_id = %v, want %d", got.TargetLaunchID, second)
	}
	if got.AttemptCount != 2 {
		t.Fatalf("attempt_count after replacement attach = %d, want 2", got.AttemptCount)
	}
}
