package bidding

import (
	"testing"

	jobdb "github.com/osteele/weft/internal/db"
)

func TestLoadInstanceOutcomes_IncludesPreRunningFailuresWithProviderInstance(t *testing.T) {
	database := jobdb.SetupTestDB(t)

	preRunningID, err := jobdb.CreateLaunch(database, &jobdb.Launch{
		Status:           jobdb.LaunchStatusLaunching,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX 4090",
		CostPerHourCents: 100,
		Reliability:      0.9,
	})
	if err != nil {
		t.Fatalf("CreateLaunch(pre-running): %v", err)
	}
	if err := jobdb.SetLaunchProviderID(database, preRunningID, "offer-pre"); err != nil {
		t.Fatalf("SetLaunchProviderID(pre-running): %v", err)
	}
	if err := jobdb.UpdateLaunchStatus(database, preRunningID, jobdb.LaunchStatusFailed, jobdb.TerminationReasonProviderFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus(pre-running): %v", err)
	}

	cancelledID, err := jobdb.CreateLaunch(database, &jobdb.Launch{
		Status:           jobdb.LaunchStatusLaunching,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX 4090",
		CostPerHourCents: 100,
		Reliability:      0.9,
	})
	if err != nil {
		t.Fatalf("CreateLaunch(cancelled): %v", err)
	}
	if err := jobdb.UpdateLaunchStatus(database, cancelledID, jobdb.LaunchStatusCancelled, jobdb.TerminationReasonCancelled); err != nil {
		t.Fatalf("UpdateLaunchStatus(cancelled): %v", err)
	}

	outcomes, err := LoadInstanceOutcomes(database)
	if err != nil {
		t.Fatalf("LoadInstanceOutcomes: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("LoadInstanceOutcomes returned %d outcomes, want 1", len(outcomes))
	}
	if outcomes[0].TerminationReason != jobdb.TerminationReasonProviderFailure {
		t.Fatalf("TerminationReason = %q, want %q", outcomes[0].TerminationReason, jobdb.TerminationReasonProviderFailure)
	}

	model := BuildSurvivalModel(outcomes)
	if model == nil {
		t.Fatal("BuildSurvivalModel returned nil")
	}
	gs := model.Global["vastai"]
	if gs == nil || gs.Total != 1 || gs.Survived != 0 {
		t.Fatalf("vastai global survival = %+v, want {Survived:0 Total:1}", gs)
	}
}
