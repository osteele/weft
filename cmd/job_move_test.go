package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/orchestration"
)

func TestRefreshLaunchableJobs_ReloadsAfterUnplace(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "python train.py", "move regression")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	staleJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if staleJob == nil {
		t.Fatal("expected job to exist")
	}
	if !staleJob.HasAssignedHost() {
		t.Fatalf("expected stale job to be host-assigned, got target=%s host=%q", staleJob.TargetKind(), staleJob.Host)
	}

	if err := db.MoveQueuedJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("move queued job to unplaced: %v", err)
	}

	// This is the stale-struct behavior that caused `job move ... new` to fail.
	staleGroups := campaign.PrepareGroups([]*db.Job{staleJob}, database, "", nil)
	if len(staleGroups) != 0 {
		t.Fatalf("stale groups = %d, want 0", len(staleGroups))
	}

	launchable, warnings := orchestration.RefreshLaunchableJobs(database, []*db.Job{staleJob})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if len(launchable) != 1 {
		t.Fatalf("launchable jobs = %d, want 1", len(launchable))
	}
	if launchable[0].HasAssignedHost() {
		t.Fatalf("reloaded job still appears assigned: host=%q target=%s", launchable[0].Host, launchable[0].TargetKind())
	}

	freshGroups := campaign.PrepareGroups(launchable, database, "", nil)
	if len(freshGroups) != 1 {
		t.Fatalf("fresh groups = %d, want 1", len(freshGroups))
	}
}
