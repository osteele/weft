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
