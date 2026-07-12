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
	if _, err := db.CreateMoveTargetAttempt(database, intent.ID, jobID, "", &launchID, db.StatusQueued); err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
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
	if ps.Move == nil {
		t.Fatal("expected move display")
	}
	if ps.Move.Phase != "waiting for destination acceptance" {
		t.Fatalf("move phase = %q", ps.Move.Phase)
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

// Regression: once a launch's agent has been ready, queued jobs belong in
// BucketQueued — even if no job is currently active. This guards against the
// wi3501-style misclassification where an instance in a between-jobs phase
// (uploading outputs, post_job_uploads_drained, ready_for_next_job) had its
// queued follow-up jobs reappear in the Launching section.
func TestPlacementStatusForJobs_QueuedOnceLaunchAgentEverReady(t *testing.T) {
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
	if err := db.SetLaunchAgentReadyAtIfUnset(database, launchID, time.Unix(9_000, 0)); err != nil {
		t.Fatalf("SetLaunchAgentReadyAtIfUnset: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	got, err := PlacementStatusForJobs(database, []*db.Job{job}, time.Unix(10_000, 0))
	if err != nil {
		t.Fatalf("PlacementStatusForJobs: %v", err)
	}
	if ps := got[jobID]; ps.Bucket != BucketQueued {
		t.Fatalf("bucket = %q, want %q (ever-ready launch should not bucket queued jobs as launching)", ps.Bucket, BucketQueued)
	}
}

func TestExpandJobsForOpenMovesShowsTerminalMoveAttemptsAsMoveRows(t *testing.T) {
	sourceLaunchID := int64(5001)
	targetLaunchID := int64(5003)
	sourceAttemptID := int64(36855)
	targetAttemptID := int64(36857)
	job := &db.Job{
		ID:          4333,
		Status:      db.StatusQueued,
		Project:     "adjective-order",
		Description: "rental duplicate",
	}

	got := ExpandJobsForOpenMoves([]*db.Job{job}, map[int64]PlacementStatus{
		4333: {
			JobID: 4333,
			Move: &MoveDisplay{
				IntentID:        531,
				State:           db.MoveIntentStateOpen,
				SourceAttemptID: &sourceAttemptID,
				TargetAttemptID: &targetAttemptID,
				SourceLabel:     "wi5001",
				TargetLabel:     "wi5003",
				AttemptsByID: map[int64]db.JobAttempt{
					sourceAttemptID: {
						ID:            sourceAttemptID,
						JobID:         4333,
						AttemptNumber: 2,
						LaunchID:      &sourceLaunchID,
						Status:        db.StatusCanceled,
						EndTime:       int64Ptr(9_000),
						CloudOutcome:  db.AttemptOutcomeOrphaned,
					},
					targetAttemptID: {
						ID:            targetAttemptID,
						JobID:         4333,
						AttemptNumber: 3,
						LaunchID:      &targetLaunchID,
						Status:        db.StatusCanceled,
						EndTime:       int64Ptr(9_000),
					},
				},
			},
		},
	})

	if len(got) != 2 {
		t.Fatalf("expanded jobs = %d, want source and target move rows: %+v", len(got), got)
	}
	source, target := got[0], got[1]
	if !source.DisplayMoveDim {
		t.Fatalf("source row should be dimmed: %+v", source)
	}
	if source.Status != db.StatusQueued {
		t.Fatalf("source status = %q, want queued display status for infra-closed source", source.Status)
	}
	if source.EndTime != nil || source.ExitCode != nil || source.FailureReason != "" {
		t.Fatalf("source row should not expose internal closure as user-terminal status: %+v", source)
	}
	if target.DisplayMoveDim {
		t.Fatalf("target row should be active/full contrast: %+v", target)
	}
	if target.Status != db.StatusQueued {
		t.Fatalf("target status = %q, want queued display status while move remains open", target.Status)
	}
	if target.LaunchID == nil || *target.LaunchID != targetLaunchID {
		t.Fatalf("target launch = %v, want %d", target.LaunchID, targetLaunchID)
	}
	if target.EndTime != nil || target.ExitCode != nil || target.FailureReason != "" {
		t.Fatalf("target row should not expose terminal attempt outcome while move remains open: %+v", target)
	}
}

func TestExpandJobsForOpenMovesSuppressesFailedSourceContext(t *testing.T) {
	sourceLaunchID := int64(5001)
	targetLaunchID := int64(5003)
	sourceAttemptID := int64(36855)
	targetAttemptID := int64(36857)
	job := &db.Job{
		ID:          4333,
		Status:      db.StatusQueued,
		Project:     "adjective-order",
		Description: "rental duplicate",
	}

	got := ExpandJobsForOpenMoves([]*db.Job{job}, map[int64]PlacementStatus{
		4333: {
			JobID: 4333,
			Move: &MoveDisplay{
				IntentID:         531,
				State:            db.MoveIntentStateOpen,
				SourceAttemptID:  &sourceAttemptID,
				TargetAttemptID:  &targetAttemptID,
				SourceLabel:      "wi5001",
				TargetLabel:      "wi5003",
				SourceLaunchDead: true,
				AttemptsByID: map[int64]db.JobAttempt{
					sourceAttemptID: {
						ID:            sourceAttemptID,
						JobID:         4333,
						AttemptNumber: 2,
						LaunchID:      &sourceLaunchID,
						Status:        db.StatusCanceled,
						EndTime:       int64Ptr(9_000),
						CloudOutcome:  db.AttemptOutcomeOrphaned,
					},
					targetAttemptID: {
						ID:            targetAttemptID,
						JobID:         4333,
						AttemptNumber: 3,
						LaunchID:      &targetLaunchID,
						Status:        db.StatusQueued,
					},
				},
			},
		},
	})

	if len(got) != 1 {
		t.Fatalf("expanded rows = %d, want only target row when source launch failed: %+v", len(got), got)
	}
	if got[0].DisplayMoveDim {
		t.Fatalf("remaining row should be active target, got dim source: %+v", got[0])
	}
	if got[0].LaunchID == nil || *got[0].LaunchID != targetLaunchID {
		t.Fatalf("remaining row launch = %v, want target launch %d", got[0].LaunchID, targetLaunchID)
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}
