package orchestration

import (
	"testing"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestRecordAutoPilotReuseDecision(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "exp", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         "NVIDIA",
		GPUMemGB:         48,
		ResolvedGPUName:  "RTX_4090",
		CostPerHourCents: 42,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	recordAutoPilotReuseDecision(database, job,
		campaign.InstanceCapacity{Instance: inst, RunningJobCount: 2},
		[]blockreason.ReuseRejection{{Instance: "wi4421", Reason: "gpu too small"}})

	got, err := db.LatestPlacementDecisionForJob(database, jobID)
	if err != nil {
		t.Fatalf("LatestPlacementDecisionForJob: %v", err)
	}
	if got == nil {
		t.Fatal("expected a reuse decision, got nil")
	}
	if got.Operation != "autopilot_reuse" || got.SelectedKind != "reuse-instance" {
		t.Fatalf("decision identity = %+v", got)
	}
	if got.Details.ColocatedBehind != 2 {
		t.Fatalf("ColocatedBehind = %d, want 2", got.Details.ColocatedBehind)
	}
	if len(got.Details.Rejected) != 1 || got.Details.Rejected[0] != "wi4421: gpu too small" {
		t.Fatalf("rejected = %v", got.Details.Rejected)
	}
}

func TestRecordLaunchDecisions(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "exp", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         "NVIDIA",
		GPUMemGB:         80,
		ResolvedGPUName:  "A100",
		CostPerHourCents: 120,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.ClaimJobForLaunch(database, jobID, instanceID); err != nil {
		t.Fatalf("ClaimJobForLaunch: %v", err)
	}

	// operation is parameterized so the quicklaunch (TUI) caller is
	// distinguishable from the autopilot caller in the recorded decision.
	recordLaunchDecisions(database, []int64{instanceID}, "quicklaunch", "launched on demand")

	got, err := db.LatestPlacementDecisionForJob(database, jobID)
	if err != nil {
		t.Fatalf("LatestPlacementDecisionForJob: %v", err)
	}
	if got == nil {
		t.Fatal("expected a launch decision, got nil")
	}
	if got.Operation != "quicklaunch" || got.SelectedKind != "launch-instance" {
		t.Fatalf("decision identity = %+v", got)
	}
	if got.Details.CostPerHour != "$1.20/hr" {
		t.Fatalf("CostPerHour = %q, want $1.20/hr", got.Details.CostPerHour)
	}
	if got.Details.Why != "launched on demand" {
		t.Fatalf("Why = %q, want \"launched on demand\"", got.Details.Why)
	}
}
