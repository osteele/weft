package jobview

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestPlacementStatusForJobs_OpenIntentUsesPlacingBucketAndIntentTime(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	intent, err := db.CreatePlacementIntent(database, jobID, "auto_relaunch")
	if err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}
	const intentCreatedAt = int64(9_000)
	if _, err := database.Exec(`UPDATE placement_intents SET created_at = ? WHERE id = ?`, intentCreatedAt, intent.ID); err != nil {
		t.Fatalf("backdate placement intent: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	got, err := PlacementStatusForJobs(database, []*db.Job{job}, time.Unix(10_000, 0))
	if err != nil {
		t.Fatalf("PlacementStatusForJobs: %v", err)
	}
	ps := got[jobID]
	if ps.Bucket != BucketPlacing {
		t.Fatalf("bucket = %q, want %q", ps.Bucket, BucketPlacing)
	}
	if !ps.HasOpenIntent {
		t.Fatalf("HasOpenIntent = false, want true")
	}
	if ps.DisplayAt != intentCreatedAt {
		t.Fatalf("DisplayAt = %d, want %d", ps.DisplayAt, intentCreatedAt)
	}
}

func TestPlacementStatusForJobs_OpenMoveIntentUsesReplacementLaunchTime(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "runpod"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     db.MoveTargetNew,
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

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	got, err := PlacementStatusForJobs(database, []*db.Job{job}, time.Unix(10_000, 0))
	if err != nil {
		t.Fatalf("PlacementStatusForJobs: %v", err)
	}
	ps := got[jobID]
	if ps.Bucket != BucketPlacing {
		t.Fatalf("bucket = %q, want %q", ps.Bucket, BucketPlacing)
	}
	if ps.DisplayAt != launchCreatedAt {
		t.Fatalf("DisplayAt = %d, want %d", ps.DisplayAt, launchCreatedAt)
	}
}

func TestPlacementStatusForJobs_LaunchingUntilTargetInstanceHasActiveJob(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	got, err := PlacementStatusForJobs(database, []*db.Job{job}, time.Unix(10_000, 0))
	if err != nil {
		t.Fatalf("PlacementStatusForJobs: %v", err)
	}
	if ps := got[jobID]; ps.Bucket != BucketLaunching {
		t.Fatalf("bucket = %q, want %q", ps.Bucket, BucketLaunching)
	}
}
