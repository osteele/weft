package bidding

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
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

func TestBuildSurvivalModel_RecencyDecay(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	old := now.Add(-90 * 24 * time.Hour) // ~4 half-lives ago: weight ≈ 1/16
	mkOutcome := func(reason string, endedAt time.Time) InstanceOutcome {
		return InstanceOutcome{
			Provider:          testProvider,
			TerminationReason: reason,
			CostPerHourCents:  100,
			ResolvedGPUName:   "RTX 4090",
			Reliability:       0.95,
			EndedAtUnix:       endedAt.Unix(),
		}
	}

	// 14 stale failures, no recent observations.
	var outcomes []InstanceOutcome
	for range 14 {
		outcomes = append(outcomes, mkOutcome("provider_failure", old))
	}

	stale := BuildSurvivalModelAt(outcomes, now)
	staleSurv := stale.OfferSurvival(cloud.Offer{Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 1.00, Reliability: 0.95})

	// Without decay, 14 failures and a 0.95 prior give ~38% (below the 40%
	// default floor — wj1238's exact situation). With decay applied at ~4
	// half-lives, the weighted total shrinks to ~14/16 = 0.88 observations,
	// the global rate reverts toward the reliability prior, and the
	// posterior recovers above the floor — i.e. the offer becomes eligible
	// to be probed again, which is the whole point of the change.
	if staleSurv <= 0.40 {
		t.Errorf("stale failures should decay above the 40%% offer floor; got %.3f", staleSurv)
	}

	// Sanity check: same outcomes evaluated immediately (no age) should still
	// look bad.
	freshNow := old
	fresh := BuildSurvivalModelAt(outcomes, freshNow)
	freshSurv := fresh.OfferSurvival(cloud.Offer{Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 1.00, Reliability: 0.95})
	if freshSurv > 0.5 {
		t.Errorf("fresh failures should give a low posterior; got %.3f (want <= 0.5)", freshSurv)
	}
	if !(freshSurv < staleSurv) {
		t.Errorf("stale posterior (%.3f) should be higher than fresh posterior (%.3f) — decay not applied", staleSurv, freshSurv)
	}
}
