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

	weftBugID, err := jobdb.CreateLaunch(database, &jobdb.Launch{
		Status:           jobdb.LaunchStatusLaunching,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX 4090",
		CostPerHourCents: 100,
		Reliability:      0.9,
	})
	if err != nil {
		t.Fatalf("CreateLaunch(weft_bug): %v", err)
	}
	if err := jobdb.UpdateLaunchStatus(database, weftBugID, jobdb.LaunchStatusFailed, jobdb.TerminationReasonWeftBug); err != nil {
		t.Fatalf("UpdateLaunchStatus(weft_bug): %v", err)
	}

	outcomes, err := LoadInstanceOutcomes(database)
	if err != nil {
		t.Fatalf("LoadInstanceOutcomes: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("LoadInstanceOutcomes returned %d outcomes, want 1 (cancelled and weft_bug must both be excluded)", len(outcomes))
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

	// Without decay, 14 failures and a 0.95 prior are far below the 40%
	// default floor. With decay applied at ~4
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

func TestBuildSurvivalModel_Bad4090VRAMBucketFallsBelowDefaultFloor(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	endedAt := now.Add(-1 * time.Hour).Unix()
	mk := func(reason string) InstanceOutcome {
		return InstanceOutcome{
			Provider:          testProvider,
			TerminationReason: reason,
			CostPerHourCents:  52,
			ResolvedGPUName:   "RTX 4090",
			GPUMemGB:          48,
			Reliability:       0.95,
			EndedAtUnix:       endedAt,
		}
	}

	var outcomes []InstanceOutcome
	for range 15 {
		outcomes = append(outcomes, mk("completed"))
	}
	for range 29 {
		outcomes = append(outcomes, mk("infra_failure"))
	}

	model := BuildSurvivalModelAt(outcomes, now)
	surv := model.OfferSurvival(cloud.Offer{
		Provider:    testProvider,
		GPUName:     "RTX 4090",
		GPUMemGB:    48,
		CostPerHour: 0.52,
		Reliability: 0.95,
	})

	if surv >= 0.40 {
		t.Fatalf("48GB 4090 bucket survival = %.3f, want below default 40%% floor", surv)
	}
}

func TestBuildSurvivalModel_SeparatesVRAMVariants(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	endedAt := now.Add(-1 * time.Hour).Unix()
	mk := func(reason string, vram int) InstanceOutcome {
		return InstanceOutcome{
			Provider:          testProvider,
			TerminationReason: reason,
			CostPerHourCents:  100,
			ResolvedGPUName:   "RTX 4090",
			GPUMemGB:          vram,
			Reliability:       0.95,
			EndedAtUnix:       endedAt,
		}
	}
	var outcomes []InstanceOutcome
	// 4090 24GB: all survived (good SKU, plenty of supply)
	for range 10 {
		outcomes = append(outcomes, mk("completed", 24))
	}
	// 4090 48GB: all failed (rare SKU, concentrated geography)
	for range 10 {
		outcomes = append(outcomes, mk("provider_failure", 48))
	}

	model := BuildSurvivalModelAt(outcomes, now)

	good := model.OfferSurvival(cloud.Offer{Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 1.00, GPUMemGB: 24, Reliability: 0.95})
	bad := model.OfferSurvival(cloud.Offer{Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 1.00, GPUMemGB: 48, Reliability: 0.95})

	if good < 0.85 {
		t.Errorf("4090 24GB SKU should still look healthy after 10 successes; got %.3f", good)
	}
	if bad > 0.5 {
		t.Errorf("4090 48GB SKU should look unhealthy after 10 failures; got %.3f", bad)
	}
	if good-bad < 0.4 {
		t.Errorf("VRAM variants should produce distinct posteriors; good=%.3f bad=%.3f", good, bad)
	}
}

func TestBuildSurvivalModel_HierarchicalGeoPenalty(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	endedAt := now.Add(-1 * time.Hour).Unix()
	mk := func(reason, dc, machineID string) InstanceOutcome {
		return InstanceOutcome{
			Provider:          testProvider,
			TerminationReason: reason,
			CostPerHourCents:  100,
			ResolvedGPUName:   "RTX 4090",
			GPUMemGB:          24,
			Reliability:       0.95,
			DataCenter:        dc,
			MachineID:         machineID,
			EndedAtUnix:       endedAt,
		}
	}
	var outcomes []InstanceOutcome
	// Background: 30 successful runs in Texas, US (good country, good region).
	for range 30 {
		outcomes = append(outcomes, mk("completed", "Texas, US", "tx-1"))
	}
	// Bad region cluster: 15 failures all in Sichuan, CN, machine s1.
	for range 15 {
		outcomes = append(outcomes, mk("provider_failure", "Sichuan, CN", "s1"))
	}
	// Healthy machine in healthy country: 5 successes in Texas, US, machine tx-2.
	for range 5 {
		outcomes = append(outcomes, mk("completed", "Texas, US", "tx-2"))
	}

	model := BuildSurvivalModelAt(outcomes, now)

	tx := cloud.Offer{Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 1.00, GPUMemGB: 24, Reliability: 0.95, DataCenter: "Texas, US", MachineID: "tx-2"}
	sichuan := cloud.Offer{Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 1.00, GPUMemGB: 24, Reliability: 0.95, DataCenter: "Sichuan, CN", MachineID: "s1"}

	txSurv := model.OfferSurvival(tx)
	cnSurv := model.OfferSurvival(sichuan)

	// The two offers share the same SKU bucket (RTX 4090 24GB at $1.00),
	// so both get the same group-level posterior; the hierarchical
	// adjustment is what should drive them apart.
	if txSurv < 0.65 {
		t.Errorf("good region/machine should look healthy after geo adjustment; got %.3f", txSurv)
	}
	if cnSurv > 0.4 {
		t.Errorf("bad region/machine should look unhealthy after geo adjustment; got %.3f", cnSurv)
	}
	if txSurv-cnSurv < 0.4 {
		t.Errorf("hierarchical penalty should produce distinct posteriors; tx=%.3f cn=%.3f", txSurv, cnSurv)
	}
}

func TestBuildSurvivalModel_GeoCascadeAvoidsDoubleCount(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	endedAt := now.Add(-1 * time.Hour).Unix()
	mk := func(reason, dc, machineID string) InstanceOutcome {
		return InstanceOutcome{
			Provider: testProvider, TerminationReason: reason,
			CostPerHourCents: 100, ResolvedGPUName: "RTX 4090",
			GPUMemGB: 24, Reliability: 0.95,
			DataCenter: dc, MachineID: machineID, EndedAtUnix: endedAt,
		}
	}
	// All failures concentrated in one region on one machine. Both the
	// region penalty and the machine penalty would individually attribute
	// the badness; the cascade should count it once.
	var outcomes []InstanceOutcome
	for range 20 {
		outcomes = append(outcomes, mk("completed", "Texas, US", "tx-1"))
	}
	for range 10 {
		outcomes = append(outcomes, mk("provider_failure", "Bad Region, XX", "bad-1"))
	}
	model := BuildSurvivalModelAt(outcomes, now)

	bad := cloud.Offer{Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 1.00, GPUMemGB: 24, Reliability: 0.95, DataCenter: "Bad Region, XX", MachineID: "bad-1"}
	got := model.OfferSurvival(bad)

	// A double-counting implementation (region penalty AND machine penalty
	// both against global) would produce a value below ~0.05 (≈0.2 × 0.2).
	// The cascade attributes the badness once, so the floor should be
	// well above 0.05 — somewhere around the regional posterior itself
	// (single ratio, not squared).
	if got < 0.05 {
		t.Errorf("cascade is double-counting the regional/machine signal; got %.3f", got)
	}
}
