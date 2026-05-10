package db

import (
	"testing"
	"time"
)

func TestReconcilePlacementRows_RestoresNoStartMoveTargetToLiveSource(t *testing.T) {
	database := setupTestDB(t)
	now := time.Unix(10_000, 0)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 4101, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 4101, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          4101,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &dst,
		AttemptCount:   1,
		MaxAttempts:    4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := TransferJobLaunchID(database, 4101, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}

	result, err := ReconcilePlacementRows(database, PlacementReconcileOptions{Now: now, ProtectionWindow: time.Minute})
	if err != nil {
		t.Fatalf("ReconcilePlacementRows: %v", err)
	}
	if result.JobsUpdated != 1 {
		t.Fatalf("JobsUpdated = %d, want 1", result.JobsUpdated)
	}
	job, err := GetJobByID(database, 4101)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != src {
		t.Fatalf("job launch_id = %v, want source %d", job.LaunchID, src)
	}
	gotIntent, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != MoveIntentStateOpen {
		t.Fatalf("move intent state = %q, want open", gotIntent.State)
	}

	second, err := ReconcilePlacementRows(database, PlacementReconcileOptions{Now: now.Add(time.Second), ProtectionWindow: time.Minute})
	if err != nil {
		t.Fatalf("second ReconcilePlacementRows: %v", err)
	}
	if second.JobsUpdated != 0 || second.IntentsResolved != 1 {
		t.Fatalf("second result = %+v, want stale restored-source intent resolution", second)
	}
	gotIntent, err = GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent second: %v", err)
	}
	if gotIntent.State != MoveIntentStateCanceled {
		t.Fatalf("move intent state after second reconcile = %q, want canceled", gotIntent.State)
	}
}

func TestReconcilePlacementRows_DeadSourceReturnsMoveTargetToUnplaced(t *testing.T) {
	database := setupTestDB(t)
	now := time.Unix(20_000, 0)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 4102, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 4102, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          4102,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &dst,
		AttemptCount:   4,
		MaxAttempts:    4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := UpdateLaunchStatus(database, src, LaunchStatusFailed, TerminationReasonProviderFailure, "source failed"); err != nil {
		t.Fatalf("UpdateLaunchStatus source: %v", err)
	}
	if err := TransferJobLaunchID(database, 4102, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}

	result, err := ReconcilePlacementRows(database, PlacementReconcileOptions{Now: now, ProtectionWindow: time.Minute})
	if err != nil {
		t.Fatalf("ReconcilePlacementRows: %v", err)
	}
	if result.JobsUpdated != 1 || result.IntentsResolved != 1 {
		t.Fatalf("result = %+v, want one job update and one intent resolution", result)
	}
	job, err := GetJobByID(database, 4102)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID != nil {
		t.Fatalf("job launch_id = %v, want unplaced", job.LaunchID)
	}
	gotIntent, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != MoveIntentStateCanceled {
		t.Fatalf("move intent state = %q, want canceled", gotIntent.State)
	}
}

func TestReconcilePlacementRows_StartedMoveTargetConfirmsIntent(t *testing.T) {
	database := setupTestDB(t)
	now := time.Unix(30_000, 0)

	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 4103, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 4103, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          4103,
		SourceLaunchID: &src,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &dst,
		AttemptCount:   1,
		MaxAttempts:    4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := TransferJobLaunchID(database, 4103, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET start_time = ? WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`,
		now.Unix()-10, 4103, dst,
	); err != nil {
		t.Fatalf("mark target started: %v", err)
	}

	result, err := ReconcilePlacementRows(database, PlacementReconcileOptions{Now: now, ProtectionWindow: time.Minute})
	if err != nil {
		t.Fatalf("ReconcilePlacementRows: %v", err)
	}
	if result.IntentsResolved != 1 {
		t.Fatalf("IntentsResolved = %d, want 1", result.IntentsResolved)
	}
	gotIntent, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != MoveIntentStateConfirmed {
		t.Fatalf("move intent state = %q, want confirmed", gotIntent.State)
	}
	job, err := GetJobByID(database, 4103)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != dst {
		t.Fatalf("job launch_id = %v, want target %d", job.LaunchID, dst)
	}
}

func TestReconcilePlacementRows_PlacementIntentsConfirmAndCancel(t *testing.T) {
	database := setupTestDB(t)
	now := time.Unix(40_000, 0)

	live, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch live: %v", err)
	}
	insertTestJob(t, database, 4104, "python train.py", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 4104, live); err != nil {
		t.Fatalf("SetJobLaunchID live: %v", err)
	}
	confirmedIntent, err := CreatePlacementIntent(database, 4104, "test-confirm")
	if err != nil {
		t.Fatalf("CreatePlacementIntent confirm: %v", err)
	}

	insertTestJob(t, database, 4105, "python train.py", "/tmp", StatusQueued)
	canceledIntent, err := CreatePlacementIntent(database, 4105, "test-cancel")
	if err != nil {
		t.Fatalf("CreatePlacementIntent cancel: %v", err)
	}
	if _, err := database.Exec(`UPDATE placement_intents SET created_at = ? WHERE id = ?`, now.Add(-2*time.Minute).Unix(), canceledIntent.ID); err != nil {
		t.Fatalf("age placement intent: %v", err)
	}

	result, err := ReconcilePlacementRows(database, PlacementReconcileOptions{Now: now, ProtectionWindow: time.Minute})
	if err != nil {
		t.Fatalf("ReconcilePlacementRows: %v", err)
	}
	if result.IntentsResolved != 2 {
		t.Fatalf("IntentsResolved = %d, want 2", result.IntentsResolved)
	}
	gotConfirmed, err := GetPlacementIntent(database, confirmedIntent.ID)
	if err != nil {
		t.Fatalf("GetPlacementIntent confirm: %v", err)
	}
	if gotConfirmed.State != PlacementIntentStateConfirmed {
		t.Fatalf("confirmed intent state = %q, want confirmed", gotConfirmed.State)
	}
	gotCanceled, err := GetPlacementIntent(database, canceledIntent.ID)
	if err != nil {
		t.Fatalf("GetPlacementIntent cancel: %v", err)
	}
	if gotCanceled.State != PlacementIntentStateCanceled {
		t.Fatalf("canceled intent state = %q, want canceled", gotCanceled.State)
	}
}

func TestReconcilePlacementRows_StaleCloudHostReturnsUnplaced(t *testing.T) {
	database := setupTestDB(t)
	now := time.Unix(50_000, 0)

	terminal, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch terminal: %v", err)
	}
	insertTestJob(t, database, 4106, "python train.py", "/tmp", StatusQueued)
	if _, err := database.Exec(
		`UPDATE job_attempts SET host = ?, launch_id = NULL WHERE job_id = ? AND end_time IS NULL`,
		LaunchHost(terminal), 4106,
	); err != nil {
		t.Fatalf("set stale host: %v", err)
	}

	result, err := ReconcilePlacementRows(database, PlacementReconcileOptions{Now: now, ProtectionWindow: time.Minute})
	if err != nil {
		t.Fatalf("ReconcilePlacementRows: %v", err)
	}
	if result.JobsUpdated != 1 {
		t.Fatalf("JobsUpdated = %d, want 1", result.JobsUpdated)
	}
	job, err := GetJobByID(database, 4106)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Host != "" || job.LaunchID != nil {
		t.Fatalf("job placement = host %q launch %v, want unplaced", job.Host, job.LaunchID)
	}
}
