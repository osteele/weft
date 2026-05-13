package db

import (
	"testing"
	"time"
)

func TestPlacementStatusForJobs_OpenIntentUsesPlacingBucketAndIntentTime(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 5101, "python train.py", "/tmp", StatusQueued)
	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, 5101, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	intent, err := CreatePlacementIntent(database, 5101, "auto_relaunch")
	if err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}
	const intentCreatedAt = int64(9_000)
	if _, err := database.Exec(`UPDATE placement_intents SET created_at = ? WHERE id = ?`, intentCreatedAt, intent.ID); err != nil {
		t.Fatalf("backdate placement intent: %v", err)
	}

	job, err := GetJobByID(database, 5101)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	got, err := PlacementStatusForJobs(database, []*Job{job}, time.Unix(10_000, 0))
	if err != nil {
		t.Fatalf("PlacementStatusForJobs: %v", err)
	}
	ps := got[5101]
	if ps.Bucket != PlacementBucketPlacing {
		t.Fatalf("bucket = %q, want %q", ps.Bucket, PlacementBucketPlacing)
	}
	if !ps.HasOpenIntent {
		t.Fatalf("HasOpenIntent = false, want true")
	}
	if ps.DisplayAt != intentCreatedAt {
		t.Fatalf("DisplayAt = %d, want %d", ps.DisplayAt, intentCreatedAt)
	}
}

func TestPlacementStatusForJobs_OpenMoveIntentUsesReplacementLaunchTime(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 5103, "python train.py", "/tmp", StatusQueued)
	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "runpod"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, 5103, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          5103,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &launchID,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}

	const intentCreatedAt = int64(9_000)
	const launchCreatedAt = int64(9_700)
	if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, intentCreatedAt, intent.ID); err != nil {
		t.Fatalf("backdate move intent: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET created_at = ? WHERE id = ?`, launchCreatedAt, launchID); err != nil {
		t.Fatalf("set launch created_at: %v", err)
	}

	job, err := GetJobByID(database, 5103)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	got, err := PlacementStatusForJobs(database, []*Job{job}, time.Unix(10_000, 0))
	if err != nil {
		t.Fatalf("PlacementStatusForJobs: %v", err)
	}
	ps := got[5103]
	if ps.Bucket != PlacementBucketPlacing {
		t.Fatalf("bucket = %q, want %q", ps.Bucket, PlacementBucketPlacing)
	}
	if ps.DisplayAt != launchCreatedAt {
		t.Fatalf("DisplayAt = %d, want %d", ps.DisplayAt, launchCreatedAt)
	}
}

func TestPlacementStatusForJobs_LaunchingUntilTargetInstanceHasActiveJob(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 5102, "python train.py", "/tmp", StatusQueued)
	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, 5102, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, err := GetJobByID(database, 5102)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	got, err := PlacementStatusForJobs(database, []*Job{job}, time.Unix(10_000, 0))
	if err != nil {
		t.Fatalf("PlacementStatusForJobs: %v", err)
	}
	if ps := got[5102]; ps.Bucket != PlacementBucketLaunching {
		t.Fatalf("bucket = %q, want %q", ps.Bucket, PlacementBucketLaunching)
	}
}
