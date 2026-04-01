package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func TestSelectLaunchGroups_FiltersSelectedJobs(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "H100",
			GPUMemGB: 80,
			Jobs: []*db.Job{
				{ID: 1},
				{ID: 2},
			},
		},
		{
			GPUClass: "A100",
			GPUMemGB: 40,
			Jobs: []*db.Job{
				{ID: 3},
			},
		},
	}

	selectedGroups, requestedJobs := selectLaunchGroups(groups, map[int64]bool{
		2: true,
		3: true,
	})

	if requestedJobs != 2 {
		t.Fatalf("requestedJobs = %d, want 2", requestedJobs)
	}
	if len(selectedGroups) != 2 {
		t.Fatalf("selectedGroups = %d, want 2", len(selectedGroups))
	}
	if len(selectedGroups[0].Jobs) != 1 || selectedGroups[0].Jobs[0].ID != 2 {
		t.Fatalf("first group jobs = %#v, want only job 2", selectedGroups[0].Jobs)
	}
	if len(selectedGroups[1].Jobs) != 1 || selectedGroups[1].Jobs[0].ID != 3 {
		t.Fatalf("second group jobs = %#v, want only job 3", selectedGroups[1].Jobs)
	}
}

func TestPrepareLaunchExecutionPlan_RevalidatesReusableInstances(t *testing.T) {
	database := db.SetupTestDB(t)

	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusCompleted,
		Provider:        "vastai",
		GPUClass:        "NVIDIA",
		ResolvedGPUName: "RTX 3090",
		GPUMemGB:        24,
	}); err != nil {
		t.Fatalf("CreateLaunch(completed): %v", err)
	}

	client := &cloud.MockClient{
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{
					ProviderID:  "fresh-offer",
					Provider:    cloud.ProviderVastai,
					GPUName:     "RTX 3090",
					GPUMemGB:    24,
					CostPerHour: 0.50,
					DLPerf:      10,
				},
			}, nil
		},
	}

	prep, err := prepareLaunchExecutionPlan(
		database,
		[]cloud.Client{client},
		nil,
		[]campaign.InstanceGroup{{
			GPUClass: "NVIDIA",
			Jobs:     []*db.Job{{ID: 17}},
		}},
		map[int64]bool{17: true},
		bidding.StrategyCheap.Profile(),
		0,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("prepareLaunchExecutionPlan: %v", err)
	}

	if len(prep.StrategyPlan.ReuseAssignments) != 0 {
		t.Fatalf("reuse assignments = %d, want 0", len(prep.StrategyPlan.ReuseAssignments))
	}
	if len(prep.LaunchGroups) != 1 {
		t.Fatalf("launch groups = %d, want 1", len(prep.LaunchGroups))
	}
	if len(prep.Offers) != 1 || prep.Offers[0].ProviderID != "fresh-offer" {
		t.Fatalf("offers = %#v, want fresh-offer", prep.Offers)
	}
}

func TestLaunchModelUpdate_PlanReadyStartsInlineWatchAfterRegistration(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusLaunching,
		Provider: "vastai",
		GPUSpec:  "RTX 3090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	model, _ := launchModel{
		launching:               true,
		inlineWatchEnabled:      true,
		database:                database,
		appConfig:               &config.Config{},
		registeredInstanceIDSet: make(map[int64]struct{}),
	}.Update(launchInstanceRegisteredMsg{instanceID: instanceID})
	got := model.(launchModel)
	if got.inlineWatchUsed {
		t.Fatal("inline watch started before the launch plan was ready")
	}

	model, cmd := got.Update(launchExecutionPlanMsg{expectedInstanceCount: 1})
	got = model.(launchModel)
	if !got.inlineWatchUsed {
		t.Fatal("expected inline watch to start once plan and registrations aligned")
	}
	if got.inlineWatch == nil {
		t.Fatal("expected inline watch model")
	}
	if cmd == nil {
		t.Fatal("expected inline watch init command")
	}
}
