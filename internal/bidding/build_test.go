package bidding

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	jobdb "github.com/osteele/weft/internal/db"
)

func TestLoadInstanceOutcomes_IncludesPreCreationFailures(t *testing.T) {
	database := jobdb.SetupTestDB(t)

	withProviderID, err := jobdb.CreateLaunch(database, &jobdb.Launch{
		Status:           jobdb.LaunchStatusLaunching,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX 4090",
		CostPerHourCents: 100,
		Reliability:      0.9,
	})
	if err != nil {
		t.Fatalf("CreateLaunch(with provider id): %v", err)
	}
	if err := jobdb.SetLaunchProviderID(database, withProviderID, "offer-pre"); err != nil {
		t.Fatalf("SetLaunchProviderID(with provider id): %v", err)
	}
	if err := jobdb.UpdateLaunchStatus(database, withProviderID, jobdb.LaunchStatusFailed, jobdb.TerminationReasonProviderFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus(with provider id): %v", err)
	}

	withoutProviderID, err := jobdb.CreateLaunch(database, &jobdb.Launch{
		Status:           jobdb.LaunchStatusLaunching,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX 4090",
		CostPerHourCents: 100,
		Reliability:      0.9,
		MachineID:        "machine-precreate",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(without provider id): %v", err)
	}
	if err := jobdb.UpdateLaunchStatus(database, withoutProviderID, jobdb.LaunchStatusFailed, jobdb.TerminationReasonProviderFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus(without provider id): %v", err)
	}

	accountFailureID, err := jobdb.CreateLaunch(database, &jobdb.Launch{
		Status:           jobdb.LaunchStatusLaunching,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX 4090",
		CostPerHourCents: 100,
		Reliability:      0.9,
		MachineID:        "machine-account",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(account failure): %v", err)
	}
	if err := jobdb.UpdateLaunchStatus(database, accountFailureID, jobdb.LaunchStatusFailed, jobdb.TerminationReasonInfraFailure, "instance creation failed: provider returned empty response while account credit was exhausted"); err != nil {
		t.Fatalf("UpdateLaunchStatus(account failure): %v", err)
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
	if len(outcomes) != 2 {
		t.Fatalf("LoadInstanceOutcomes returned %d outcomes, want 2 (cancelled and weft_bug must both be excluded)", len(outcomes))
	}
	for i, outcome := range outcomes {
		if outcome.TerminationReason != jobdb.TerminationReasonProviderFailure {
			t.Fatalf("outcome %d TerminationReason = %q, want %q", i, outcome.TerminationReason, jobdb.TerminationReasonProviderFailure)
		}
		if outcome.SurvivalClass != SurvivalTrainProviderFailure {
			t.Fatalf("outcome %d SurvivalClass = %q, want %q", i, outcome.SurvivalClass, SurvivalTrainProviderFailure)
		}
	}

	model := BuildSurvivalModel(outcomes)
	if model == nil {
		t.Fatal("BuildSurvivalModel returned nil")
	}
	gs := model.Global["vastai"]
	if gs == nil || gs.Total != 2 || gs.Survived != 0 {
		t.Fatalf("vastai global survival = %+v, want {Survived:0 Total:2}", gs)
	}
}

func TestLoadInstanceOutcomes_ExcludesAccountCreditExhausted(t *testing.T) {
	database := jobdb.SetupTestDB(t)

	creditID, err := jobdb.CreateLaunch(database, &jobdb.Launch{
		Status:           jobdb.LaunchStatusLaunching,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX 4090",
		CostPerHourCents: 100,
		Reliability:      0.9,
		MachineID:        "machine-credit",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(credit): %v", err)
	}
	if err := jobdb.UpdateLaunchStatus(database, creditID, jobdb.LaunchStatusFailed, jobdb.TerminationReasonProviderFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus(credit, initial): %v", err)
	}
	if err := jobdb.ReclassifyLaunchTerminationReason(database, creditID,
		jobdb.TerminationReasonAccountCreditExhausted,
		"reclassified as account_credit_exhausted at 2026-05-31T00:00:00Z"); err != nil {
		t.Fatalf("ReclassifyLaunchTerminationReason: %v", err)
	}

	survivingID, err := jobdb.CreateLaunch(database, &jobdb.Launch{
		Status:           jobdb.LaunchStatusLaunching,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX 4090",
		CostPerHourCents: 100,
		Reliability:      0.9,
		MachineID:        "machine-real",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(survivor): %v", err)
	}
	if err := jobdb.UpdateLaunchStatus(database, survivingID, jobdb.LaunchStatusFailed, jobdb.TerminationReasonProviderFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus(survivor): %v", err)
	}

	outcomes, err := LoadInstanceOutcomes(database)
	if err != nil {
		t.Fatalf("LoadInstanceOutcomes: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("LoadInstanceOutcomes returned %d outcomes, want 1 (account_credit_exhausted must be excluded)", len(outcomes))
	}
	if outcomes[0].TerminationReason != jobdb.TerminationReasonProviderFailure {
		t.Fatalf("outcome.TerminationReason = %q, want %q",
			outcomes[0].TerminationReason, jobdb.TerminationReasonProviderFailure)
	}
}

func TestLoadInstanceOutcomes_ClassifiesLegacyNonTrainableRunpodFailures(t *testing.T) {
	database := jobdb.SetupTestDB(t)

	create := func(name string, reason string, detail string, diskGB int) int64 {
		t.Helper()
		id, err := jobdb.CreateLaunch(database, &jobdb.Launch{
			Status:           jobdb.LaunchStatusLaunching,
			Provider:         string(cloud.ProviderRunpod),
			ResolvedGPUName:  "RTX 4090",
			GPUMemGB:         24,
			DiskGB:           diskGB,
			CostPerHourCents: 34,
			Reliability:      0.95,
		})
		if err != nil {
			t.Fatalf("CreateLaunch(%s): %v", name, err)
		}
		if err := jobdb.UpdateLaunchStatus(database, id, jobdb.LaunchStatusFailed, reason, detail); err != nil {
			t.Fatalf("UpdateLaunchStatus(%s): %v", name, err)
		}
		return id
	}

	create("old runpodctl flag", jobdb.TerminationReasonInfraFailure,
		"instance creation failed: create pod: pod create --gpu-id: unknown flag: --min-cuda-version", 1037937)
	create("manifest upload", jobdb.TerminationReasonInfraFailure,
		"manifest upload failed: put object campaigns/4099/manifest.json: operation error S3: PutObject: EOF", 50)
	create("impossible disk request", jobdb.TerminationReasonInfraFailure,
		"instance creation failed: offer unavailable: pod create --gpu-id: There are no longer any instances available with enough disk space.", 265779)
	create("setup stall", jobdb.TerminationReasonPhaseStall,
		"setup phase stalled for 34h31m28s — terminating instance", 189)
	create("stale offer", jobdb.TerminationReasonInfraFailure,
		"instance creation failed: offer unavailable: pod create --gpu-id: This machine does not have the resources to deploy your pod. Please try a different machine", 163)

	completedID, err := jobdb.CreateLaunch(database, &jobdb.Launch{
		Status:           jobdb.LaunchStatusLaunching,
		Provider:         string(cloud.ProviderRunpod),
		ResolvedGPUName:  "RTX 4090",
		GPUMemGB:         24,
		DiskGB:           85,
		CostPerHourCents: 34,
		Reliability:      0.95,
	})
	if err != nil {
		t.Fatalf("CreateLaunch(completed): %v", err)
	}
	if err := jobdb.UpdateLaunchStatus(database, completedID, jobdb.LaunchStatusCompleted, jobdb.TerminationReasonCompleted); err != nil {
		t.Fatalf("UpdateLaunchStatus(completed): %v", err)
	}

	outcomes, err := LoadInstanceOutcomes(database)
	if err != nil {
		t.Fatalf("LoadInstanceOutcomes: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("LoadInstanceOutcomes returned %d outcomes, want 2 trainable rows", len(outcomes))
	}

	classes := map[SurvivalTrainingClass]int{}
	for _, outcome := range outcomes {
		classes[outcome.SurvivalClass]++
	}
	if classes[SurvivalTrainProviderFailure] != 1 {
		t.Fatalf("provider-failure training rows = %d, want 1", classes[SurvivalTrainProviderFailure])
	}
	if classes[SurvivalTrainSurvived] != 1 {
		t.Fatalf("survived training rows = %d, want 1", classes[SurvivalTrainSurvived])
	}
}

func TestClassifySurvivalTrainingOutcome(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		detail string
		diskGB int
		want   SurvivalTrainingClass
	}{
		{
			name:   "completed",
			reason: jobdb.TerminationReasonCompleted,
			want:   SurvivalTrainSurvived,
		},
		{
			name:   "provider stale offer",
			reason: jobdb.TerminationReasonInfraFailure,
			detail: "This machine does not have the resources to deploy your pod",
			diskGB: 163,
			want:   SurvivalTrainProviderFailure,
		},
		{
			name:   "runpodctl bug",
			reason: jobdb.TerminationReasonInfraFailure,
			detail: "unknown flag: --min-cuda-version",
			diskGB: 1037937,
			want:   SurvivalTrainExcludedWeftBug,
		},
		{
			name:   "manifest upload",
			reason: jobdb.TerminationReasonInfraFailure,
			detail: "manifest upload failed: put object campaigns/4099/manifest.json",
			want:   SurvivalTrainExcludedLocalStaging,
		},
		{
			name:   "impossible disk",
			reason: jobdb.TerminationReasonInfraFailure,
			detail: "There are no longer any instances available with enough disk space.",
			diskGB: 265779,
			want:   SurvivalTrainExcludedInvalidRequest,
		},
		{
			name:   "normal disk stockout remains trainable",
			reason: jobdb.TerminationReasonInfraFailure,
			detail: "There are no longer any instances available with enough disk space.",
			diskGB: 163,
			want:   SurvivalTrainProviderFailure,
		},
		{
			name:   "setup stall",
			reason: jobdb.TerminationReasonPhaseStall,
			detail: "setup phase stalled for 34h31m28s",
			want:   SurvivalTrainExcludedSetup,
		},
		{
			name:   "legacy credit detail",
			reason: jobdb.TerminationReasonProviderFailure,
			detail: "provider returned empty response while account credit was exhausted",
			want:   SurvivalTrainExcludedAccountCredit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifySurvivalTrainingOutcome(tt.reason, tt.detail, tt.diskGB)
			if got != tt.want {
				t.Fatalf("ClassifySurvivalTrainingOutcome() = %q, want %q", got, tt.want)
			}
		})
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
