package orchestration

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
)

func TestMoveQueuedJobToNewInstanceRejectsExternalJobBeforeMutation(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "PENDING", NormalizedStatus: db.StatusQueued, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	var attemptsBefore int
	if err := database.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id = ?`, binding.JobID).Scan(&attemptsBefore); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	_, err = MoveQueuedJobToNewInstance(database, binding.JobID, false)
	if err == nil || !strings.Contains(err.Error(), "external executor") {
		t.Fatalf("MoveQueuedJobToNewInstance error = %v, want external refusal", err)
	}
	var attemptsAfter, intents int
	if err := database.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id = ?`, binding.JobID).Scan(&attemptsAfter); err != nil {
		t.Fatalf("count attempts after: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM move_intents WHERE job_id = ?`, binding.JobID).Scan(&intents); err != nil {
		t.Fatalf("count move intents: %v", err)
	}
	if attemptsAfter != attemptsBefore || intents != 0 {
		t.Fatalf("move mutated external job: attempts=%d/%d intents=%d", attemptsAfter, attemptsBefore, intents)
	}
}

// TestGroupOutcomeTracker_PartitionsByEvent verifies the orchestration-side
// matching of campaign per-group events back to the user-visible jobs in
// each group: LaunchEventGroupDone marks those jobs placed, GroupFailed
// marks them unplaced with the reason, and any group whose outcome event
// never arrived defaults to "launch outcome not reported" so the receipt
// can never silently drop a requested instance.
func TestGroupOutcomeTracker_PartitionsByEvent(t *testing.T) {
	placedGroup := campaign.InstanceGroup{GPUClass: "NVIDIA", Jobs: []*db.Job{{ID: 2384}}}
	failedGroup := campaign.InstanceGroup{GPUClass: "NVIDIA", Jobs: []*db.Job{{ID: 2383}}}
	silentGroup := campaign.InstanceGroup{GPUClass: "NVIDIA", Jobs: []*db.Job{{ID: 2387}}}

	tracker := newGroupOutcomeTracker([]campaign.InstanceGroup{placedGroup, failedGroup, silentGroup})
	tracker.observe(campaign.LaunchEvent{Kind: campaign.LaunchEventGroupDone, Group: placedGroup})
	tracker.observe(campaign.LaunchEvent{Kind: campaign.LaunchEventGroupFailed, Group: failedGroup, Reason: "no offer accepted"})

	placed, unplaced := tracker.partition()
	if len(placed) != 1 || placed[0] != 2384 {
		t.Fatalf("placed = %v, want [2384]", placed)
	}
	wantUnplaced := map[int64]string{
		2383: "no offer accepted",
		2387: "launch outcome not reported",
	}
	if len(unplaced) != len(wantUnplaced) {
		t.Fatalf("unplaced = %v, want %d entries", unplaced, len(wantUnplaced))
	}
	for _, u := range unplaced {
		if got, ok := wantUnplaced[u.JobID]; !ok {
			t.Fatalf("unexpected unplaced job %d", u.JobID)
		} else if got != u.Reason {
			t.Fatalf("unplaced %d reason = %q, want %q", u.JobID, u.Reason, got)
		}
	}
}

// TestGroupOutcomeTracker_IgnoresUnknownGroup confirms the tracker doesn't
// crash or pollute its tally when the campaign layer emits an event for a
// group that wasn't part of the originally submitted launch — defensive
// behavior against future event-source changes.
func TestGroupOutcomeTracker_IgnoresUnknownGroup(t *testing.T) {
	knownGroup := campaign.InstanceGroup{GPUClass: "NVIDIA", Jobs: []*db.Job{{ID: 10}}}
	unknownGroup := campaign.InstanceGroup{GPUClass: "NVIDIA", Jobs: []*db.Job{{ID: 99}}}
	tracker := newGroupOutcomeTracker([]campaign.InstanceGroup{knownGroup})
	tracker.observe(campaign.LaunchEvent{Kind: campaign.LaunchEventGroupDone, Group: unknownGroup})
	tracker.observe(campaign.LaunchEvent{Kind: campaign.LaunchEventGroupDone, Group: knownGroup})

	placed, unplaced := tracker.partition()
	if len(placed) != 1 || placed[0] != 10 {
		t.Fatalf("placed = %v, want [10]", placed)
	}
	if len(unplaced) != 0 {
		t.Fatalf("unplaced = %v, want none", unplaced)
	}
}

// TestUnplacedJobsFromGroup_PreservesJobIDsAndShareReason covers the small
// helper that fans an offer-search drop reason out to every job in the
// affected group. Concretely: when SearchOffers returns nothing for a
// group, all its jobs share the same "no offers" reason in the receipt.
func TestUnplacedJobsFromGroup_PreservesJobIDsAndShareReason(t *testing.T) {
	group := campaign.InstanceGroup{
		Jobs: []*db.Job{{ID: 100}, {ID: 101}, nil, {ID: 0}, {ID: 102}},
	}
	got := unplacedJobsFromGroup(group, "no offers")
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (nil and zero-id skipped)", len(got))
	}
	for i, want := range []int64{100, 101, 102} {
		if got[i].JobID != want {
			t.Fatalf("got[%d].JobID = %d, want %d", i, got[i].JobID, want)
		}
		if got[i].Reason != "no offers" {
			t.Fatalf("got[%d].Reason = %q, want %q", i, got[i].Reason, "no offers")
		}
	}
}

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

func TestRefreshMoveToNewJobs_AcceptsPendingPlacement(t *testing.T) {
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

	launchable, warnings := refreshMoveToNewJobs(database, []*db.Job{staleJob}, false)
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

func TestBuildOptions_NewOffersRespectJobMaxComputeCap(t *testing.T) {
	job := &db.Job{
		ID:            1838,
		Status:        db.StatusQueued,
		GPUClass:      "NVIDIA",
		MaxComputeCap: "9.0",
	}
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(c cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{
					ProviderID:  "blackwell",
					Provider:    cloud.ProviderVastai,
					GPUName:     "RTX PRO 6000 WS",
					CostPerHour: 0.10,
				},
				{
					ProviderID:  "ampere",
					Provider:    cloud.ProviderVastai,
					GPUName:     "RTX A6000",
					CostPerHour: 0.20,
				},
			}, nil
		},
	}

	options, err := BuildOptions([]cloud.Client{client}, job, nil, nil, 0, 0)
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	foundA6000 := false
	for _, opt := range options {
		if !opt.IsNew {
			continue
		}
		if opt.GPUName == "RTX PRO 6000 WS" {
			t.Fatalf("incompatible Blackwell offer was returned: %+v", opt)
		}
		if opt.GPUName == "RTX A6000" {
			foundA6000 = true
		}
	}
	if !foundA6000 {
		t.Fatalf("expected compatible RTX A6000 option, got %+v", options)
	}
}

func TestBuildOptions_ExistingInstanceWaitIncludesActiveAndQueuedJobs(t *testing.T) {
	job := &db.Job{
		ID:       1933,
		Status:   db.StatusQueued,
		GPUClass: "nvidia",
	}
	capacity := campaign.InstanceCapacity{
		Instance: &db.Launch{
			ID:               2871,
			Status:           db.LaunchStatusRunning,
			GPUClass:         "nvidia",
			ResolvedGPUName:  "RTX 4070S Ti",
			CostPerHourCents: 10,
		},
		RunningJobCount: 1,
	}

	options, err := BuildOptions(nil, job, []campaign.InstanceCapacity{capacity}, map[int64]int{2871: 1}, 0, 0)
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("options len = %d, want 1: %+v", len(options), options)
	}
	if got, want := options[0].WaitTime, time.Hour; got != want {
		t.Fatalf("existing instance wait = %s, want %s", got, want)
	}
	if got, want := options[0].QueueDepth, 2; got != want {
		t.Fatalf("existing instance queue depth = %d, want %d", got, want)
	}
	if !options[0].Eligible {
		t.Fatalf("existing instance option should be eligible: %+v", options[0])
	}
}

func TestBuildExistingOptionsIncludesIneligibleInstances(t *testing.T) {
	memGB := 20
	job := &db.Job{
		ID:       1934,
		Status:   db.StatusQueued,
		GPUClass: "nvidia",
		GPUMemGB: &memGB,
	}
	capacity := campaign.InstanceCapacity{
		Instance: &db.Launch{
			ID:               2872,
			Status:           db.LaunchStatusRunning,
			GPUClass:         "nvidia",
			ResolvedGPUName:  "RTX 2080 Ti",
			GPUMemGB:         11,
			CostPerHourCents: 0,
		},
		RunningJobCount: 0,
	}

	options := BuildExistingOptions(job, []campaign.InstanceCapacity{capacity}, nil, 0)
	if len(options) != 1 {
		t.Fatalf("options len = %d, want 1: %+v", len(options), options)
	}
	if options[0].Eligible {
		t.Fatalf("incompatible instance should be ineligible: %+v", options[0])
	}
	if strings.TrimSpace(options[0].Reason) == "" {
		t.Fatalf("ineligible instance reason is empty: %+v", options[0])
	}
}

func TestBuildOnPremOptionsIncludesEligibleAndIneligibleHosts(t *testing.T) {
	inventory.UseTestHosts(t)
	database := db.SetupTestDB(t)
	for _, host := range []string{"host-alpha", "host-beta", "host-gamma"} {
		placement.RecordContentionObs(database, host, &placement.HostMetrics{
			CPUPercent:    5,
			GPUPercent:    5,
			QueueDepth:    0,
			GPUJobsQueued: 0,
		})
	}
	memGB := 20
	job := &db.Job{
		ID:       1935,
		Status:   db.StatusQueued,
		GPUClass: "nvidia",
		GPUMemGB: &memGB,
	}

	options := BuildOnPremOptions(database, nil, job)
	if len(options) != 3 {
		t.Fatalf("options len = %d, want 3: %+v", len(options), options)
	}
	var eligible, ineligible bool
	for _, opt := range options {
		if opt.Kind != OptionKindOnPrem {
			t.Fatalf("option kind = %q, want on-prem: %+v", opt.Kind, opt)
		}
		if opt.Eligible {
			eligible = true
		} else if strings.TrimSpace(opt.Reason) != "" {
			ineligible = true
		}
	}
	if !eligible {
		t.Fatalf("expected at least one eligible on-prem host: %+v", options)
	}
	if !ineligible {
		t.Fatalf("expected at least one ineligible on-prem host with reason: %+v", options)
	}
}

func TestBuildOptionsWithSurvival_FiltersLowSurvivalMachine(t *testing.T) {
	job := &db.Job{
		ID:       1905,
		Status:   db.StatusQueued,
		GPUClass: "A100",
	}
	badMachine := "machine-bad"
	goodMachine := "machine-good"
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{
					ProviderID:  "bad",
					Provider:    cloud.ProviderVastai,
					GPUName:     "A100 PCIE",
					GPUMemGB:    80,
					CostPerHour: 0.10,
					Reliability: 0.99,
					MachineID:   badMachine,
				},
				{
					ProviderID:  "good",
					Provider:    cloud.ProviderVastai,
					GPUName:     "A100 PCIE",
					GPUMemGB:    80,
					CostPerHour: 0.20,
					Reliability: 0.99,
					MachineID:   goodMachine,
				},
			}, nil
		},
	}

	now := time.Now()
	outcomes := []bidding.InstanceOutcome{
		{Provider: cloud.ProviderVastai, TerminationReason: db.TerminationReasonCompleted, ResolvedGPUName: "A100 PCIE", GPUMemGB: 80, CostPerHourCents: 20, Reliability: 0.99, MachineID: goodMachine, EndedAtUnix: now.Unix()},
		{Provider: cloud.ProviderVastai, TerminationReason: db.TerminationReasonCompleted, ResolvedGPUName: "A100 PCIE", GPUMemGB: 80, CostPerHourCents: 20, Reliability: 0.99, MachineID: goodMachine, EndedAtUnix: now.Unix()},
		{Provider: cloud.ProviderVastai, TerminationReason: db.TerminationReasonCompleted, ResolvedGPUName: "A100 PCIE", GPUMemGB: 80, CostPerHourCents: 20, Reliability: 0.99, MachineID: goodMachine, EndedAtUnix: now.Unix()},
		{Provider: cloud.ProviderVastai, TerminationReason: db.TerminationReasonInfraFailure, ResolvedGPUName: "A100 PCIE", GPUMemGB: 80, CostPerHourCents: 10, Reliability: 0.99, MachineID: badMachine, EndedAtUnix: now.Unix()},
		{Provider: cloud.ProviderVastai, TerminationReason: db.TerminationReasonInfraFailure, ResolvedGPUName: "A100 PCIE", GPUMemGB: 80, CostPerHourCents: 10, Reliability: 0.99, MachineID: badMachine, EndedAtUnix: now.Unix()},
		{Provider: cloud.ProviderVastai, TerminationReason: db.TerminationReasonInfraFailure, ResolvedGPUName: "A100 PCIE", GPUMemGB: 80, CostPerHourCents: 10, Reliability: 0.99, MachineID: badMachine, EndedAtUnix: now.Unix()},
	}
	model := bidding.BuildSurvivalModelAt(outcomes, now)

	options, err := BuildOptionsWithSurvival([]cloud.Client{client}, job, nil, nil, 0, 0, model, 0.4, nil)
	if err != nil {
		t.Fatalf("BuildOptionsWithSurvival: %v", err)
	}
	for _, opt := range options {
		if opt.IsNew && opt.Offer != nil && opt.Offer.MachineID == badMachine {
			t.Fatalf("low-survival machine was returned: %+v", opt.Offer)
		}
	}
	foundGood := false
	for _, opt := range options {
		if opt.IsNew && opt.Offer != nil && opt.Offer.MachineID == goodMachine {
			foundGood = true
		}
	}
	if !foundGood {
		t.Fatalf("expected surviving offer from good machine, got %+v", options)
	}
}

func TestBuildOptionsWithSurvival_ExcludesFailedMoveMachine(t *testing.T) {
	job := &db.Job{
		ID:       1905,
		Status:   db.StatusQueued,
		GPUClass: "A100",
	}
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{ProviderID: "same-machine-new-offer", Provider: cloud.ProviderVastai, GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 0.10, MachineID: "56764"},
				{ProviderID: "other-machine", Provider: cloud.ProviderVastai, GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 0.20, MachineID: "99999"},
			}, nil
		},
	}
	excluded := map[string]struct{}{"56764": {}}

	options, err := BuildOptionsWithSurvival([]cloud.Client{client}, job, nil, nil, 0, 0, nil, 0, excluded)
	if err != nil {
		t.Fatalf("BuildOptionsWithSurvival: %v", err)
	}
	for _, opt := range options {
		if opt.IsNew && opt.Offer != nil && opt.Offer.MachineID == "56764" {
			t.Fatalf("excluded machine was returned: %+v", opt.Offer)
		}
	}
	foundOther := false
	for _, opt := range options {
		if opt.IsNew && opt.Offer != nil && opt.Offer.MachineID == "99999" {
			foundOther = true
		}
	}
	if !foundOther {
		t.Fatalf("expected offer from other machine, got %+v", options)
	}
}

func TestBuildNewOptionsWithSurvivalAndTelemetry_ExcludesTorchPreflightMachine(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "torch job", "L40S")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	failedLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusFailed, Provider: "vastai", MachineID: "bad-machine"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", &failedLaunch, db.StatusFailed); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET failure_reason = ? WHERE job_id = ? AND launch_id = ?`,
		db.FailureReasonInfraTorchPreflightFailed, jobID, failedLaunch,
	); err != nil {
		t.Fatalf("mark torch preflight failure: %v", err)
	}
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{ProviderID: "same-machine-new-offer", Provider: cloud.ProviderVastai, GPUName: "L40S", GPUMemGB: 48, CostPerHour: 0.10, MachineID: "bad-machine", Reliability: 0.99},
				{ProviderID: "other-machine", Provider: cloud.ProviderVastai, GPUName: "L40S", GPUMemGB: 48, CostPerHour: 0.20, MachineID: "good-machine", Reliability: 0.99},
			}, nil
		},
	}

	options, err := BuildNewOptionsWithSurvivalAndTelemetry(database, []cloud.Client{client}, job, 0, nil, 0, nil)
	if err != nil {
		t.Fatalf("BuildNewOptionsWithSurvivalAndTelemetry: %v", err)
	}
	for _, opt := range options {
		if opt.IsNew && opt.Offer != nil && opt.Offer.MachineID == "bad-machine" {
			t.Fatalf("torch-preflight failed machine was returned: %+v", opt.Offer)
		}
	}
	foundGood := false
	for _, opt := range options {
		if opt.IsNew && opt.Offer != nil && opt.Offer.MachineID == "good-machine" {
			foundGood = true
		}
	}
	if !foundGood {
		t.Fatalf("expected offer from good machine, got %+v", options)
	}
}

func TestBuildNewOptionsWithSurvivalAndTelemetry_RecordsOfferSnapshot(t *testing.T) {
	database := db.SetupTestDB(t)
	job := &db.Job{
		ID:       1906,
		Status:   db.StatusQueued,
		GPUClass: "A100",
	}
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{ProviderID: "vast-1", Provider: cloud.ProviderVastai, GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 0.20, Reliability: 0.99},
			}, nil
		},
	}

	options, err := BuildNewOptionsWithSurvivalAndTelemetry(database, []cloud.Client{client}, job, 0.95, nil, 0, nil)
	if err != nil {
		t.Fatalf("BuildNewOptionsWithSurvivalAndTelemetry: %v", err)
	}
	if len(options) == 0 {
		t.Fatal("expected at least one move-to-new option")
	}
	var count, offers int
	var details string
	if err := database.QueryRow(`
		SELECT COUNT(*), COALESCE(MAX(offer_count), 0), COALESCE(MAX(details_json), '')
		FROM offer_availability_snapshots
		WHERE gpu_class = 'A100'`,
	).Scan(&count, &offers, &details); err != nil {
		t.Fatalf("query offer snapshot: %v", err)
	}
	if count != 1 || offers != 1 {
		t.Fatalf("snapshot count=%d offers=%d, want count=1 offers=1", count, offers)
	}
	if !strings.Contains(details, `"provider_offer_counts":{"vastai":1}`) {
		t.Fatalf("details_json = %s, want vastai provider count", details)
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
	// before the target accepts the hidden destination attempt. See
	// unplaceIfNeeded.
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

func TestOpenBulkMoveIntentsRestoresRetryableNoStartFailureToSource(t *testing.T) {
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "rental job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	intentIDs, err := openMoveIntentsForNewInstanceGroups(database, []campaign.InstanceGroup{{GPUClass: "A100", Jobs: []*db.Job{job}}}, []cloud.Offer{{Provider: cloud.ProviderVastai, ProviderID: "offer-1", GPUName: "A100"}})
	if err != nil {
		t.Fatalf("openMoveIntentsForNewInstanceGroups: %v", err)
	}
	if intentIDs[jobID] == 0 {
		t.Fatal("expected move intent id for job")
	}
	if err := db.UpdateMoveIntentTargetLaunch(database, intentIDs[jobID], dst); err != nil {
		t.Fatalf("UpdateMoveIntentTargetLaunch: %v", err)
	}
	if err := db.TransferJobLaunchID(database, jobID, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}

	n, err := db.ResetLaunchJobs(database, dst, db.AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset count = %d, want 1", n)
	}
	after, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after: %v", err)
	}
	if after.LaunchID == nil || *after.LaunchID != src {
		t.Fatalf("after launch_id = %v, want source %d while retry budget remains", after.LaunchID, src)
	}
	intent, err := db.GetMoveIntent(database, intentIDs[jobID])
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if intent.State != db.MoveIntentStateOpen {
		t.Fatalf("intent state = %q, want open", intent.State)
	}
}

func TestOpenBulkMoveIntentsSupersedesPriorOpenMoveIntent(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "cool100", t.TempDir(), "python train.py", "rental job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	prior, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:      jobID,
		TargetKind: db.MoveTargetNew,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent prior: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	intentIDs, err := openMoveIntentsForNewInstanceGroups(database, []campaign.InstanceGroup{{GPUClass: "A100", Jobs: []*db.Job{job}}}, []cloud.Offer{{Provider: cloud.ProviderVastai, ProviderID: "offer-2", GPUName: "A100"}})
	if err != nil {
		t.Fatalf("openMoveIntentsForNewInstanceGroups: %v", err)
	}
	if intentIDs[jobID] == 0 || intentIDs[jobID] == prior.ID {
		t.Fatalf("new intent id = %d, prior = %d", intentIDs[jobID], prior.ID)
	}
	gotPrior, err := db.GetMoveIntent(database, prior.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent prior: %v", err)
	}
	if gotPrior.State != db.MoveIntentStateCanceled || gotPrior.Resolution != "superseded by explicit move" {
		t.Fatalf("prior intent = (%s, %q), want canceled superseded", gotPrior.State, gotPrior.Resolution)
	}
	gotNew, err := db.GetMoveIntent(database, intentIDs[jobID])
	if err != nil {
		t.Fatalf("GetMoveIntent new: %v", err)
	}
	if gotNew.State != db.MoveIntentStateOpen {
		t.Fatalf("new intent state = %q, want open", gotNew.State)
	}
}

func TestOpenBulkMoveIntentsSupersedesPriorOpenPlacementIntent(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "cool100", t.TempDir(), "python train.py", "rental job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	prior, err := db.CreatePlacementIntent(database, jobID, "bulk_move")
	if err != nil {
		t.Fatalf("CreatePlacementIntent prior: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	intentIDs, err := openMoveIntentsForNewInstanceGroups(database, []campaign.InstanceGroup{{GPUClass: "A100", Jobs: []*db.Job{job}}}, []cloud.Offer{{Provider: cloud.ProviderVastai, ProviderID: "offer-2", GPUName: "A100"}})
	if err != nil {
		t.Fatalf("openMoveIntentsForNewInstanceGroups: %v", err)
	}
	if intentIDs[jobID] == 0 {
		t.Fatal("expected new move intent id")
	}
	gotPrior, err := db.GetPlacementIntent(database, prior.ID)
	if err != nil {
		t.Fatalf("GetPlacementIntent prior: %v", err)
	}
	if gotPrior.State != db.PlacementIntentStateCanceled || gotPrior.Resolution != "superseded by explicit move" {
		t.Fatalf("prior intent = (%s, %q), want canceled superseded", gotPrior.State, gotPrior.Resolution)
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

func TestJobsForGrouping_ClearsInventoryHostWithoutMutatingSource(t *testing.T) {
	attemptID := int64(26629)
	original := &db.Job{
		ID:          1885,
		Status:      db.StatusQueued,
		Host:        "cool100",
		LatestRunID: &attemptID,
		GPUClass:    "ampere",
	}

	grouping := jobsForGrouping([]*db.Job{original})
	if len(grouping) != 1 {
		t.Fatalf("grouping len = %d, want 1", len(grouping))
	}
	if grouping[0] == original {
		t.Fatal("expected placed inventory job to be cloned for grouping")
	}
	if grouping[0].Host != "" {
		t.Fatalf("grouping host = %q, want empty", grouping[0].Host)
	}
	if grouping[0].LatestRunID == nil || *grouping[0].LatestRunID != attemptID {
		t.Fatalf("grouping LatestRunID = %v, want source attempt %d", grouping[0].LatestRunID, attemptID)
	}
	if original.Host != "cool100" {
		t.Fatalf("original host = %q, want cool100", original.Host)
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

func TestLaunchNewForJob_SelectsRankedOfferAndPassesStrategy(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, "nvidia"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}

	var gotOpt Option
	restore := stubExecuteMoveOptionForMove(t, func(
		_ context.Context,
		_ *sql.DB,
		_ *r2.Client,
		_ *config.Config,
		_ []cloud.Client,
		_ *db.Job,
		opt Option,
		_ *db.MoveIntent,
	) (string, error) {
		gotOpt = opt
		return "new RTX 3090 instance wi77", nil
	})
	defer restore()

	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			if constraints.GPUClass != "nvidia" {
				t.Fatalf("GPUClass constraint = %q, want nvidia", constraints.GPUClass)
			}
			return []cloud.Offer{
				{ProviderID: "expensive", Provider: cloud.ProviderVastai, GPUName: "A100", GPUMemGB: 40, CostPerHour: 2.00},
				{ProviderID: "cheap", Provider: cloud.ProviderVastai, GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.20},
			}, nil
		},
	}

	desc, err := LaunchNewForJob(context.Background(), database, nil, nil, []cloud.Client{client}, jobID, bidding.StrategyFast)
	if err != nil {
		t.Fatalf("LaunchNewForJob: %v", err)
	}
	if desc == "" {
		t.Fatal("desc is empty")
	}
	if gotOpt.Strategy != bidding.StrategyFast {
		t.Fatalf("strategy = %q, want %q", gotOpt.Strategy, bidding.StrategyFast)
	}
	if gotOpt.Offer == nil || gotOpt.Offer.ProviderID != "cheap" {
		t.Fatalf("selected offer = %#v, want provider_id cheap", gotOpt.Offer)
	}
}

func TestLaunchNewForJob_NoOffers(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	client := &cloud.MockClient{
		ProviderVal:      cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) { return nil, nil },
	}
	_, err = LaunchNewForJob(context.Background(), database, nil, nil, []cloud.Client{client}, jobID, bidding.StrategyFast)
	if err == nil || !strings.Contains(err.Error(), "no compatible new-instance offer") {
		t.Fatalf("LaunchNewForJob err = %v, want no-offer error", err)
	}
}

func TestLaunchNewForJob_JobNotFound(t *testing.T) {
	database := db.SetupTestDB(t)
	_, err := LaunchNewForJob(context.Background(), database, nil, nil, nil, 999, bidding.StrategyFast)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("LaunchNewForJob err = %v, want not found", err)
	}
}

func TestLaunchNewForJobRejectsExternalJobBeforeOfferLookup(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "PENDING", NormalizedStatus: db.StatusQueued, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	_, err = LaunchNewForJob(context.Background(), database, nil, nil, nil, binding.JobID, bidding.StrategyFast)
	if err == nil || !strings.Contains(err.Error(), "external executor") {
		t.Fatalf("LaunchNewForJob error = %v, want external refusal", err)
	}
	var intents, launches int
	if err := database.QueryRow(`SELECT COUNT(*) FROM move_intents WHERE job_id = ?`, binding.JobID).Scan(&intents); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM launches`).Scan(&launches); err != nil {
		t.Fatalf("count launches: %v", err)
	}
	if intents != 0 || launches != 0 {
		t.Fatalf("external launch mutated DB: intents=%d launches=%d", intents, launches)
	}
}

func TestLaunchNewForJob_JobNotQueued(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetRequestedStatus(database, jobID, db.StatusKilled); err != nil {
		t.Fatalf("SetRequestedStatus: %v", err)
	}

	_, err = LaunchNewForJob(context.Background(), database, nil, nil, nil, jobID, bidding.StrategyFast)
	if err == nil || !strings.Contains(err.Error(), "cannot launch new instance for job") {
		t.Fatalf("LaunchNewForJob err = %v, want status error", err)
	}
}

func TestLaunchNewForJob_ExecuteFailureResolvesIntentCanceled(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	execErr := errors.New("provider rejected")
	restore := stubExecuteMoveOptionForMove(t, func(
		context.Context,
		*sql.DB,
		*r2.Client,
		*config.Config,
		[]cloud.Client,
		*db.Job,
		Option,
		*db.MoveIntent,
	) (string, error) {
		return "", execErr
	})
	defer restore()

	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{{ProviderID: "offer", Provider: cloud.ProviderVastai, GPUName: "A100", GPUMemGB: 40, CostPerHour: 1}}, nil
		},
	}

	_, err = LaunchNewForJob(context.Background(), database, nil, nil, []cloud.Client{client}, jobID, bidding.StrategyFast)
	if !errors.Is(err, execErr) {
		t.Fatalf("LaunchNewForJob err = %v, want %v", err, execErr)
	}
	var state string
	if err := database.QueryRow(`SELECT state FROM move_intents WHERE job_id = ?`, jobID).Scan(&state); err != nil {
		t.Fatalf("query move intent state: %v", err)
	}
	if db.MoveIntentState(state) != db.MoveIntentStateCanceled {
		t.Fatalf("move intent state = %q, want %q", state, db.MoveIntentStateCanceled)
	}
}

func TestLaunchNewForJob_ZeroInstanceLaunchResultIsFailure(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	launchErr := errors.New("provider rejected offer")
	restore := stubLaunchCampaignForMove(t, func(
		[]cloud.Client,
		*sql.DB,
		[]campaign.InstanceGroup,
		[]cloud.Offer,
		[]campaign.CostEstimate,
		*bidding.SurvivalModel,
		campaign.LaunchOpts,
		cloud.R2Config,
		func(cloud.Provider) (cloud.CreateOpts, error),
		func(campaign.LaunchEvent),
		func(int64),
		func(campaign.InstanceGroup, int64),
	) (*campaign.LaunchResult, error) {
		return &campaign.LaunchResult{Errors: []error{launchErr}}, nil
	})
	defer restore()

	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{{ProviderID: "offer", Provider: cloud.ProviderVastai, GPUName: "A100", GPUMemGB: 40, CostPerHour: 1}}, nil
		},
	}

	_, err = LaunchNewForJob(context.Background(), database, nil, config.DefaultConfig(), []cloud.Client{client}, jobID, bidding.StrategyFast)
	if !errors.Is(err, launchErr) {
		t.Fatalf("LaunchNewForJob err = %v, want %v", err, launchErr)
	}
	if !strings.Contains(err.Error(), "no new instance launched") {
		t.Fatalf("LaunchNewForJob err = %v, want no-instance context", err)
	}
	var state string
	if err := database.QueryRow(`SELECT state FROM move_intents WHERE job_id = ?`, jobID).Scan(&state); err != nil {
		t.Fatalf("query move intent state: %v", err)
	}
	if db.MoveIntentState(state) != db.MoveIntentStateCanceled {
		t.Fatalf("move intent state = %q, want %q", state, db.MoveIntentStateCanceled)
	}
}

func TestLaunchNewForJob_TargetLaunchFailureKeepsRetryableIntentOpen(t *testing.T) {
	database := db.SetupTestDB(t)
	sourceLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, sourceLaunch); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}

	launchErr := errors.New("provider rejected offer")
	restore := stubLaunchCampaignForMove(t, func(
		_ []cloud.Client,
		database *sql.DB,
		groups []campaign.InstanceGroup,
		_ []cloud.Offer,
		_ []campaign.CostEstimate,
		_ *bidding.SurvivalModel,
		_ campaign.LaunchOpts,
		_ cloud.R2Config,
		_ func(cloud.Provider) (cloud.CreateOpts, error),
		_ func(campaign.LaunchEvent),
		_ func(int64),
		_ func(campaign.InstanceGroup, int64),
	) (*campaign.LaunchResult, error) {
		targetLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusFailed, Provider: "vastai"})
		if err != nil {
			return nil, err
		}
		intent, err := db.GetOpenMoveIntent(database, groups[0].Jobs[0].ID)
		if err != nil {
			return nil, err
		}
		if intent == nil {
			return nil, errors.New("missing open move intent")
		}
		if _, err := db.CreateMoveTargetAttempt(database, intent.ID, groups[0].Jobs[0].ID, "", &targetLaunch, db.StatusQueued); err != nil {
			return nil, err
		}
		if _, err := db.HandleMoveTargetFailedBeforeStart(database, groups[0].Jobs[0].ID, targetLaunch, db.AttemptOutcomeOrphaned); err != nil {
			return nil, err
		}
		return &campaign.LaunchResult{Errors: []error{launchErr}}, nil
	})
	defer restore()

	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{{ProviderID: "offer", Provider: cloud.ProviderVastai, GPUName: "A100", GPUMemGB: 40, CostPerHour: 1}}, nil
		},
	}

	_, err = LaunchNewForJob(context.Background(), database, nil, config.DefaultConfig(), []cloud.Client{client}, jobID, bidding.StrategyFast)
	if !errors.Is(err, launchErr) {
		t.Fatalf("LaunchNewForJob err = %v, want %v", err, launchErr)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != sourceLaunch {
		t.Fatalf("job launch_id = %v, want source %d", job.LaunchID, sourceLaunch)
	}
	intent, err := db.GetOpenMoveIntent(database, jobID)
	if err != nil {
		t.Fatalf("GetOpenMoveIntent: %v", err)
	}
	if intent == nil {
		t.Fatal("move intent = nil, want open retryable intent")
	}
	if intent.TargetLaunchID == nil || *intent.TargetLaunchID == sourceLaunch {
		t.Fatalf("target_launch_id = %v, want failed target launch", intent.TargetLaunchID)
	}
}

func TestExecuteOption_RentalToHostDispatchesAfterMoveIntent(t *testing.T) {
	database := db.SetupTestDB(t)
	sourceLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "move", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, sourceLaunch); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	before, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID before: %v", err)
	}
	sourceRunID := *before.LatestRunID

	var queuedHost string
	restoreQueue := stubQueueHostMoveJob(t, func(_ *sql.DB, job *db.Job, _ time.Duration) error {
		queuedHost = job.Host
		if job.LatestRunID == nil || *job.LatestRunID == sourceRunID {
			t.Fatalf("queued job latest run = %v, want destination attempt", job.LatestRunID)
		}
		return nil
	})
	defer restoreQueue()
	restoreTerminate := stubTerminateMoveForcedSources(t, func(*sql.DB, []SourceSnapshot, func(string)) {})
	defer restoreTerminate()

	desc, err := ExecuteOption(context.Background(), database, nil, nil, nil, jobID, Option{
		Kind:     OptionKindOnPrem,
		Host:     "cool30",
		Eligible: true,
	})
	if err != nil {
		t.Fatalf("ExecuteOption: %v", err)
	}
	if desc != "host cool30" {
		t.Fatalf("desc = %q, want host cool30", desc)
	}
	if queuedHost != "cool30" {
		t.Fatalf("queuedHost = %q, want cool30", queuedHost)
	}
	after, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after: %v", err)
	}
	if after.Host != "cool30" || after.LaunchID != nil || after.Status != db.StatusQueued {
		t.Fatalf("after = host %q launch %v status %q, want cool30 nil queued", after.Host, after.LaunchID, after.Status)
	}
	intent, err := db.GetMoveIntent(database, 1)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if intent == nil || intent.TargetHost != "cool30" || intent.State != db.MoveIntentStateConfirmed {
		t.Fatalf("intent = %+v, want confirmed target_host cool30", intent)
	}
}

func TestExecuteOption_RentalToHostQueueFailureRestoresSource(t *testing.T) {
	database := db.SetupTestDB(t)
	sourceLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "move", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, sourceLaunch); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	queueErr := errors.New("host unreachable")
	restoreQueue := stubQueueHostMoveJob(t, func(*sql.DB, *db.Job, time.Duration) error {
		return queueErr
	})
	defer restoreQueue()

	_, err = ExecuteOption(context.Background(), database, nil, nil, nil, jobID, Option{
		Kind:     OptionKindOnPrem,
		Host:     "cool30",
		Eligible: true,
	})
	if err == nil || !strings.Contains(err.Error(), "submit to host cool30") {
		t.Fatalf("ExecuteOption err = %v, want host submit error", err)
	}
	after, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after: %v", err)
	}
	if after.LaunchID == nil || *after.LaunchID != sourceLaunch {
		t.Fatalf("LaunchID = %v, want restored source %d", after.LaunchID, sourceLaunch)
	}
	intent, err := db.GetMoveIntent(database, 1)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if intent == nil || intent.State != db.MoveIntentStateCanceled {
		t.Fatalf("intent = %+v, want canceled", intent)
	}
}

func stubExecuteMoveOptionForMove(t *testing.T, fn func(context.Context, *sql.DB, *r2.Client, *config.Config, []cloud.Client, *db.Job, Option, *db.MoveIntent) (string, error)) func() {
	t.Helper()
	previous := executeMoveOptionForMove
	executeMoveOptionForMove = fn
	return func() {
		executeMoveOptionForMove = previous
	}
}

func stubLaunchCampaignForMove(t *testing.T, fn func([]cloud.Client, *sql.DB, []campaign.InstanceGroup, []cloud.Offer, []campaign.CostEstimate, *bidding.SurvivalModel, campaign.LaunchOpts, cloud.R2Config, func(cloud.Provider) (cloud.CreateOpts, error), func(campaign.LaunchEvent), func(int64), func(campaign.InstanceGroup, int64)) (*campaign.LaunchResult, error)) func() {
	t.Helper()
	previous := launchCampaignForMove
	launchCampaignForMove = fn
	return func() {
		launchCampaignForMove = previous
	}
}

func stubQueueHostMoveJob(t *testing.T, fn func(*sql.DB, *db.Job, time.Duration) error) func() {
	t.Helper()
	previous := queueHostMoveJob
	queueHostMoveJob = fn
	return func() {
		queueHostMoveJob = previous
	}
}

func stubTerminateMoveForcedSources(t *testing.T, fn func(*sql.DB, []SourceSnapshot, func(string))) func() {
	t.Helper()
	previous := terminateMoveForcedSources
	terminateMoveForcedSources = fn
	return func() {
		terminateMoveForcedSources = previous
	}
}
