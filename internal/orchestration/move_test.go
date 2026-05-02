package orchestration

import (
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestBuildMoveGroupProgressLabels_UsesOrdinalAndAnchorJobID(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "NVIDIA",
			Jobs: []*db.Job{
				{ID: 1069},
			},
		},
		{
			GPUClass: "NVIDIA",
			Jobs: []*db.Job{
				{ID: 1063},
				{ID: 1090},
			},
		},
	}

	labels := buildMoveGroupProgressLabels(groups)
	if got := moveGroupProgressLabel(groups[0], labels); got != "[1/2 wj1069]" {
		t.Fatalf("label for group 1 = %q, want %q", got, "[1/2 wj1069]")
	}
	if got := moveGroupProgressLabel(groups[1], labels); got != "[2/2 wj1063]" {
		t.Fatalf("label for group 2 = %q, want %q", got, "[2/2 wj1063]")
	}
}

func TestBuildMoveGroupProgressLabels_FallsBackToOrdinalWhenNoJobIDs(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "NVIDIA",
			Jobs:     []*db.Job{{ID: 0}, nil},
		},
	}

	labels := buildMoveGroupProgressLabels(groups)
	if got := moveGroupProgressLabel(groups[0], labels); got != "[1/1]" {
		t.Fatalf("label = %q, want %q", got, "[1/1]")
	}
}

func TestRefreshLaunchableJobs_AcceptsPendingPlacement(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "python train.py", "pending placement move")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	staleJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if err := db.MoveQueuedJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("MoveQueuedJobToUnplaced: %v", err)
	}
	if err := db.SetPendingStatus(database, jobID, db.StatusPendingPlacement); err != nil {
		t.Fatalf("SetPendingStatus: %v", err)
	}

	launchable, warnings := refreshLaunchableJobs(database, []*db.Job{staleJob})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if len(launchable) != 1 {
		t.Fatalf("launchable = %d, want 1", len(launchable))
	}
	if got := launchable[0].EffectiveStatus(); got != db.StatusPendingPlacement {
		t.Fatalf("effective status = %q, want %q", got, db.StatusPendingPlacement)
	}
}

func TestUnplaceIfNeeded_AlreadyUnplaced(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "unplaced job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.TargetKind() != db.JobTargetUnplaced {
		t.Fatalf("setup: TargetKind = %q, want %q", job.TargetKind(), db.JobTargetUnplaced)
	}

	if err := unplaceIfNeeded(database, job); err != nil {
		t.Fatalf("unplaceIfNeeded on already-unplaced job: %v", err)
	}
}

func TestUnplaceIfNeeded_RentalSourceLeftAttached(t *testing.T) {
	// Regression: bulk move-to-new must not detach rental-source jobs
	// before LaunchCampaign supersedes the claim. See unplaceIfNeeded.
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "rental job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if !job.IsRentalJob() {
		t.Fatalf("setup: IsRentalJob() = false, want true (LaunchID = %v)", job.LaunchID)
	}

	if err := unplaceIfNeeded(database, job); err != nil {
		t.Fatalf("unplaceIfNeeded on rental job: %v", err)
	}

	after, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after: %v", err)
	}
	if after.LaunchID == nil || *after.LaunchID != src {
		t.Fatalf("rental job lost its source claim: launch_id = %v, want %d", after.LaunchID, src)
	}
}

func TestJobsForGrouping_ClonesPendingPlacementAsQueued(t *testing.T) {
	pending := db.StatusPendingPlacement
	original := &db.Job{ID: 1, Status: db.StatusQueued, PendingStatus: &pending}

	grouping := jobsForGrouping([]*db.Job{original})
	if len(grouping) != 1 {
		t.Fatalf("grouping len = %d, want 1", len(grouping))
	}
	if grouping[0] == original {
		t.Fatal("expected pending job to be cloned for grouping")
	}
	if got := grouping[0].EffectiveStatus(); got != db.StatusQueued {
		t.Fatalf("grouping effective status = %q, want %q", got, db.StatusQueued)
	}
	if got := original.EffectiveStatus(); got != db.StatusPendingPlacement {
		t.Fatalf("original effective status = %q, want %q", got, db.StatusPendingPlacement)
	}
}

func TestTryRestoreJobToSource_RestoresWhenSourceAlive(t *testing.T) {
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "j", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	// Simulate the move's intermediate "unplace" step: the job loses its
	// launch association. The intent retains the source.
	if err := db.ResetJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("ResetJobToUnplaced: %v", err)
	}

	if !tryRestoreJobToSource(database, jobID, src) {
		t.Fatal("tryRestoreJobToSource returned false; expected restore to succeed")
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != src {
		t.Fatalf("after restore: launch_id = %v, want %d", job.LaunchID, src)
	}
}

func TestTryRestoreJobToSource_DoesNotRestoreWhenSourceTerminal(t *testing.T) {
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusFailed})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "j", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if tryRestoreJobToSource(database, jobID, src) {
		t.Fatal("tryRestoreJobToSource returned true; expected no restore for failed source")
	}
}

func TestTryRestoreJobToSource_NoSourceLaunchIDIsNoOp(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "j", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if tryRestoreJobToSource(database, jobID, 0) {
		t.Fatal("tryRestoreJobToSource returned true; expected no-op when source unknown")
	}
}
