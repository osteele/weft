package terminal

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/predictor"
)

func TestLaunchModelView_ShowsPartialFailures(t *testing.T) {
	m := launchModel{
		done:          true,
		campaignID:    49,
		instanceIDs:   []int64{108},
		partialErrors: []string{"RTX3090 >=20GB: create instance: quota", "A100 >=60GB: create instance: capacity"},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"Campaign 49: launched instances: 108",
		"2 planned launch(es) failed:",
		"RTX3090",
		"A100",
		"Press Enter, Esc, or q to continue.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestFormatPartialErrors_WrapsLongLines(t *testing.T) {
	out := formatPartialErrors([]string{
		"NVIDIA ≥8GB ≤12GB: search replacement offer: replacement offer price $0.08/hr exceeds 25% cap over original offer $0.06/hr",
	}, 72)

	if !strings.Contains(out, "1 planned launch(es) failed:") {
		t.Fatalf("missing header, got:\n%s", out)
	}
	for _, want := range []string{
		"replacement offer price",
		"$0.08/hr exceeds 25% cap",
		"original offer $0.06/hr",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing wrapped segment %q, got:\n%s", want, out)
		}
	}
	if strings.Count(out, "\n") < 2 {
		t.Fatalf("expected wrapped output, got:\n%s", out)
	}
}

func TestFormatTradeoffColumnHeader_ShowsRateAndTotalCost(t *testing.T) {
	header := formatTradeoffColumnHeader(campaign.CostTable{
		TimeColOffset: 14,
		TimeWidth:     12,
		RateWidth:     8,
	})

	for _, want := range []string{"option", "duration", "burn", "total cost", "instances"} {
		if !strings.Contains(header, want) {
			t.Fatalf("header missing %q, got: %q", want, header)
		}
	}
}

func TestLaunchModelView_ShowsCostPlaceholderWhileLoadingOffers(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "A100",
			GPUMemGB: 80,
			Jobs: []*db.Job{
				{ID: 7, Description: "train", WorkingDir: "/tmp/project-alpha", Project: "exp-042"},
			},
		},
	}
	items, selected, cursor := buildItemsFromGroups(groups)
	m := launchModel{
		groups:      groups,
		items:       items,
		selected:    selected,
		cursor:      cursor,
		loading:     true,
		instanceIDs: nil,
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"── Cost Estimate",
		"Searching providers for direct offers...",
		"exp-042",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestLaunchModelView_ShowsRawOfferSummaryWhilePlanning(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "A100",
			GPUMemGB: 80,
			Jobs: []*db.Job{
				{ID: 7, Description: "train", WorkingDir: "/tmp/project-alpha", Project: "exp-042"},
			},
		},
		{
			GPUClass: "H100",
			GPUMemGB: 80,
			Jobs: []*db.Job{
				{ID: 8, Description: "eval", WorkingDir: "/tmp/project-beta", Project: "exp-043"},
			},
		},
	}
	items, selected, cursor := buildItemsFromGroups(groups)
	m := launchModel{
		groups:      groups,
		items:       items,
		selected:    selected,
		cursor:      cursor,
		loading:     true,
		instanceIDs: nil,
		cachedRawOffers: []campaign.GroupRawOffers{
			{Group: groups[0], Offers: []cloud.Offer{{ProviderID: "a", Provider: cloud.ProviderVastai}}},
			{Group: groups[1]},
		},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"Direct offers found for 1/2 GPU groups.",
		"A100 ≥80GB: 1 direct offer",
		"H100 ≥80GB: 0 direct offers",
		"Building launch plan from raw offers...",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestLaunchModelView_RawOfferSummaryAggregatesDuplicateSpecs(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass:    "NVIDIA",
			GPUMemGB:    8,
			MaxGPUMemGB: 12,
			Jobs: []*db.Job{
				{ID: 1, Description: "a"},
			},
		},
		{
			GPUClass:    "NVIDIA",
			GPUMemGB:    8,
			MaxGPUMemGB: 12,
			Jobs: []*db.Job{
				{ID: 2, Description: "b"},
			},
		},
		{
			Jobs: []*db.Job{
				{ID: 3, Description: "c"},
			},
		},
	}
	items, selected, cursor := buildItemsFromGroups(groups)
	m := launchModel{
		groups:      groups,
		items:       items,
		selected:    selected,
		cursor:      cursor,
		loading:     true,
		instanceIDs: nil,
		cachedRawOffers: []campaign.GroupRawOffers{
			{Group: groups[0], Offers: make([]cloud.Offer, 64)},
			{Group: groups[1], Offers: make([]cloud.Offer, 64)},
			{Group: groups[2], Offers: make([]cloud.Offer, 64)},
		},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"NVIDIA 8-12 GB (2 groups): 64 direct offers",
		"Unspecified GPU: 64 direct offers",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NVIDIA 8-12 GB: 64 direct offers\n  NVIDIA 8-12 GB: 64 direct offers") {
		t.Fatalf("expected duplicate summary lines to be aggregated, got:\n%s", out)
	}
}

func TestLaunchModelView_RawOfferSummaryKeepsDifferentStatusesSeparate(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass:    "NVIDIA",
			GPUMemGB:    8,
			MaxGPUMemGB: 12,
			Jobs: []*db.Job{
				{ID: 1, Description: "a"},
			},
		},
		{
			GPUClass:    "NVIDIA",
			GPUMemGB:    8,
			MaxGPUMemGB: 12,
			Jobs: []*db.Job{
				{ID: 2, Description: "b"},
			},
		},
	}
	items, selected, cursor := buildItemsFromGroups(groups)
	m := launchModel{
		groups:      groups,
		items:       items,
		selected:    selected,
		cursor:      cursor,
		loading:     true,
		instanceIDs: nil,
		cachedRawOffers: []campaign.GroupRawOffers{
			{Group: groups[0], Offers: []cloud.Offer{{ProviderID: "a", Provider: cloud.ProviderVastai}}},
			{Group: groups[1]},
		},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"NVIDIA 8-12 GB: 1 direct offer",
		"NVIDIA 8-12 GB: 0 direct offers",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestLaunchModelUpdate_OffersErrorQuits(t *testing.T) {
	model, cmd := launchModel{}.Update(rawOffersLoadedMsg{err: fmt.Errorf("vastai: DNS lookup failed")})
	got := model.(launchModel)
	if got.err == nil || !strings.Contains(got.err.Error(), "DNS lookup failed") {
		t.Fatalf("expected stored error, got %v", got.err)
	}
	assertQuitCmd(t, cmd)
}

func TestLaunchModelUpdate_RawOffersKickOffPlanBuild(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "A100",
			GPUMemGB: 80,
			Jobs: []*db.Job{
				{ID: 7, Description: "train"},
			},
		},
	}
	raw := []campaign.GroupRawOffers{{Group: groups[0]}}
	reusable := []campaign.InstanceCapacity{{}}
	model, cmd := launchModel{
		groups:        groups,
		database:      db.SetupTestDB(t),
		predConfig:    &predictor.Config{},
		loading:       true,
		estimateCache: make(map[string][]campaign.CostEstimate),
	}.Update(rawOffersLoadedMsg{raw: raw, reusable: reusable})
	got := model.(launchModel)
	if got.cachedRawOffers == nil || len(got.cachedRawOffers) != 1 {
		t.Fatalf("expected cached raw offers, got %#v", got.cachedRawOffers)
	}
	if len(got.reusable) != 1 {
		t.Fatalf("expected reusable instances cached, got %d", len(got.reusable))
	}
	if !got.loading {
		t.Fatal("expected loading to remain true while plans build")
	}
	if cmd == nil {
		t.Fatal("expected follow-up plan-build command")
	}
}

func TestLaunchModelUpdate_RawOffersLoaded_IgnoresStaleRevision(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "A100",
			GPUMemGB: 80,
			Jobs:     []*db.Job{{ID: 7, Description: "train"}},
		},
	}
	existing := []campaign.GroupRawOffers{{Group: groups[0], Offers: []cloud.Offer{{ProviderID: "current", Provider: cloud.ProviderVastai}}}}
	model, cmd := launchModel{
		groups:          groups,
		groupRevision:   2,
		cachedRawOffers: existing,
	}.Update(rawOffersLoadedMsg{
		raw:      []campaign.GroupRawOffers{{Group: groups[0], Offers: []cloud.Offer{{ProviderID: "stale", Provider: cloud.ProviderRunpod}}}},
		revision: 1,
	})

	got := model.(launchModel)
	if len(got.cachedRawOffers) != 1 || got.cachedRawOffers[0].Offers[0].ProviderID != "current" {
		t.Fatalf("stale raw offers replaced current state: %#v", got.cachedRawOffers)
	}
	if cmd != nil {
		t.Fatalf("expected no follow-up command for stale raw offers, got %T", cmd)
	}
}

func TestFormatPlanProgressLines_ShowsConcurrentProfileLanes(t *testing.T) {
	var state planProgressState
	state.update(planProgressMsg{
		lane:  "cheap (1/3)",
		phase: "Estimating direct-offer costs",
	})
	state.update(planProgressMsg{
		lane:    "fast (2/3)",
		phase:   "Scoring candidate groupings",
		detail:  "split",
		current: 1,
		total:   3,
	})

	lines := formatPlanProgressLines(state)
	if len(lines) != 3 {
		t.Fatalf("line count = %d, want 3", len(lines))
	}
	for _, want := range []string{
		"Building launch plan from raw offers...",
		"cheap (1/3): Estimating direct-offer costs...",
		"fast (2/3): Scoring candidate groupings (1/3): split...",
	} {
		found := false
		for _, line := range lines {
			if line == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing progress line %q in %#v", want, lines)
		}
	}
}

func TestTradeoffProfilesForLaunchBatch_UsesEndpointsFirst(t *testing.T) {
	profiles, fullSet := tradeoffProfilesForLaunchBatch(false)
	if fullSet {
		t.Fatal("expected initial launch batch to be a partial profile set")
	}
	gotIDs := []string{profiles[0].ID, profiles[1].ID, profiles[2].ID}
	wantIDs := []string{
		bidding.StrategyCheap.Profile().ID,
		bidding.StrategyFast.Profile().ID,
		bidding.StrategyFastest.Profile().ID,
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("profile %d = %q, want %q", i, gotIDs[i], wantIDs[i])
		}
	}

	backgroundProfiles, backgroundFullSet := tradeoffProfilesForLaunchBatch(true)
	if !backgroundFullSet {
		t.Fatal("expected background launch batch to use the full profile set")
	}
	if len(backgroundProfiles) <= len(profiles) {
		t.Fatalf("background profile count = %d, want > %d", len(backgroundProfiles), len(profiles))
	}
}

func TestLaunchModelUpdate_ProfilePlansLoaded_StartsBackgroundRefinement(t *testing.T) {
	model, cmd := launchModel{
		loading:        true,
		planGeneration: 1,
		estimateCache:  make(map[string][]campaign.CostEstimate),
	}.Update(profilePlansLoadedMsg{
		plans: map[string]campaign.StrategyPlan{
			"cheap":   {},
			"fast":    {},
			"fastest": {},
		},
		options: []campaign.TradeoffOption{
			{ID: "cheap", Label: "cheap"},
			{ID: "fast", Label: "fast"},
			{ID: "fastest", Label: "fastest"},
		},
		fullSet:    false,
		generation: 1,
	})

	got := model.(launchModel)
	if got.loading {
		t.Fatal("expected loading to clear after initial tradeoff plans arrive")
	}
	if !got.refiningTradeoffs {
		t.Fatal("expected background tradeoff refinement to start after the initial profile batch")
	}
	if cmd == nil {
		t.Fatal("expected follow-up background refinement command")
	}
}

func TestLaunchModelUpdate_ProfilePlansLoaded_IgnoresStaleGeneration(t *testing.T) {
	initialPlans := map[string]campaign.StrategyPlan{
		"fast": {},
	}
	model, cmd := launchModel{
		planGeneration: 2,
		tradeoffPlans:  initialPlans,
		activeTradeoff: "fast",
	}.Update(profilePlansLoadedMsg{
		plans: map[string]campaign.StrategyPlan{
			"cheap": {},
		},
		options:    []campaign.TradeoffOption{{ID: "cheap", Label: "cheap"}},
		fullSet:    true,
		background: true,
		generation: 1,
	})

	got := model.(launchModel)
	if len(got.tradeoffPlans) != 1 {
		t.Fatalf("stale result replaced current tradeoff plans: %#v", got.tradeoffPlans)
	}
	if _, ok := got.tradeoffPlans["fast"]; !ok {
		t.Fatalf("stale result replaced current tradeoff plans: %#v", got.tradeoffPlans)
	}
	if got.activeTradeoff != "fast" {
		t.Fatalf("activeTradeoff = %q, want fast", got.activeTradeoff)
	}
	if cmd != nil {
		t.Fatalf("expected no follow-up command for stale result, got %T", cmd)
	}
}

func TestLaunchModelView_DoesNotShowNoOffersWhileTradeoffRowLoading(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 12,
			Jobs: []*db.Job{
				{ID: 580, Description: "train", WorkingDir: "/tmp/project", Project: "head-type-ontology"},
			},
		},
	}
	items, selected, cursor := buildItemsFromGroups(groups)
	m := launchModel{
		groups:          groups,
		items:           items,
		selected:        selected,
		cursor:          cursor,
		cachedRawOffers: []campaign.GroupRawOffers{{Group: groups[0]}},
		cachedTradeoffRows: []campaign.StrategySummaryRow{{
			Label:     "cheap",
			Active:    true,
			Disclosed: true,
			Loading:   true,
		}},
		costEstimates:     []campaign.CostEstimate{{}},
		tradeoffDisclosed: true,
		activeTradeoff:    "cheap",
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "estimating...") {
		t.Fatalf("expected loading tradeoff row, got:\n%s", out)
	}
	if strings.Contains(out, "No offers found for any group.") {
		t.Fatalf("unexpected no-offers detail while tradeoff row is still loading:\n%s", out)
	}
}

func TestLaunchModelUpdate_ReconcileNoJobsQuits(t *testing.T) {
	model, cmd := launchModel{reconciling: true}.Update(reconcileDoneMsg{groups: []campaign.InstanceGroup{}})
	got := model.(launchModel)
	if got.err == nil || !strings.Contains(got.err.Error(), "all placed during reconciliation") {
		t.Fatalf("expected reconciliation error, got %v", got.err)
	}
	assertQuitCmd(t, cmd)
}

func TestLaunchModelUpdate_LaunchErrorQuits(t *testing.T) {
	model, cmd := launchModel{launching: true}.Update(instancesLaunchedMsg{err: fmt.Errorf("runpod: HTTP 503")})
	got := model.(launchModel)
	if got.err == nil || !strings.Contains(got.err.Error(), "HTTP 503") {
		t.Fatalf("expected stored error, got %v", got.err)
	}
	assertQuitCmd(t, cmd)
}

func TestLaunchModelUpdate_PartialFailuresStayOpen(t *testing.T) {
	model, cmd := launchModel{launching: true}.Update(instancesLaunchedMsg{
		instanceIDs: []int64{42},
		errors:      []error{fmt.Errorf("quota exceeded")},
	})
	got := model.(launchModel)
	if !got.done {
		t.Fatal("expected done state")
	}
	if len(got.partialErrors) != 1 {
		t.Fatalf("expected 1 partial error, got %d", len(got.partialErrors))
	}
	if cmd != nil {
		t.Fatalf("expected no quit command for partial failures, got %T", cmd)
	}
}

func TestLaunchModelUpdate_OnPremDoneRefreshesGroups(t *testing.T) {
	oldGroups := []campaign.InstanceGroup{
		{GPUClass: "A100", GPUMemGB: 80, Jobs: []*db.Job{{ID: 1, Description: "old-a"}}},
		{GPUClass: "H100", GPUMemGB: 80, Jobs: []*db.Job{{ID: 2, Description: "old-b"}}},
	}
	newGroups := []campaign.InstanceGroup{
		{GPUClass: "A100", GPUMemGB: 80, Jobs: []*db.Job{{ID: 1, Description: "old-a"}}},
	}
	model, cmd := launchModel{
		groups:         oldGroups,
		groupRevision:  3,
		onPremChecking: true,
		estimateCache:  map[string][]campaign.CostEstimate{"cached": {{}}},
	}.Update(onPremDoneMsg{groups: newGroups})

	got := model.(launchModel)
	if got.onPremChecking {
		t.Fatal("expected on-prem background check to clear")
	}
	if got.groupRevision != 4 {
		t.Fatalf("groupRevision = %d, want 4", got.groupRevision)
	}
	if len(got.groups) != 1 || got.groups[0].GPUClass != "A100" {
		t.Fatalf("groups = %#v, want refreshed single group", got.groups)
	}
	if !got.loading {
		t.Fatal("expected loading to restart after on-prem refresh")
	}
	if len(got.estimateCache) != 0 {
		t.Fatalf("estimate cache = %#v, want reset", got.estimateCache)
	}
	if !strings.Contains(got.statusHint, "Placed 1 job(s) on on-prem hosts") {
		t.Fatalf("statusHint = %q, want on-prem placement notice", got.statusHint)
	}
	if cmd == nil {
		t.Fatal("expected follow-up fetch command after on-prem refresh")
	}
}

func TestLaunchModelHandleKey_EnterWaitsForOnPremBackground(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{GPUClass: "A100", GPUMemGB: 80, Jobs: []*db.Job{{ID: 1, Description: "train"}}},
	}
	items, selected, cursor := buildItemsFromGroups(groups)
	model, cmd := launchModel{
		groups:         groups,
		items:          items,
		selected:       selected,
		cursor:         cursor,
		onPremChecking: true,
		onPremPhase:    "Collecting on-prem metrics for 2 host(s)...",
	}.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	got := model.(launchModel)
	if got.launching {
		t.Fatal("expected launch to remain blocked while on-prem check is running")
	}
	if !strings.Contains(got.statusHint, "Checking on-prem placement") {
		t.Fatalf("statusHint = %q, want on-prem wait message", got.statusHint)
	}
	if cmd != nil {
		t.Fatalf("expected no command while waiting on background on-prem check, got %T", cmd)
	}
}

func TestLaunchModelView_ErrorHasNoDismissPrompt(t *testing.T) {
	m := launchModel{
		err:       fmt.Errorf("vastai: auth failed"),
		fromWatch: true,
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "Error: vastai: auth failed") {
		t.Fatalf("expected error output, got:\n%s", out)
	}
	if strings.Contains(out, "Press Enter") {
		t.Fatalf("unexpected dismiss prompt in error view:\n%s", out)
	}
}

func TestLaunchModelUpdate_StartsInlineWatchOnFirstRegistration(t *testing.T) {
	database := db.SetupTestDB(t)

	firstID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusLaunching,
		Provider: "vastai",
		GPUSpec:  "RTX 3090",
	})
	if err != nil {
		t.Fatalf("create first instance: %v", err)
	}
	secondID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusLaunching,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("create second instance: %v", err)
	}

	m := launchModel{
		launching:               true,
		expectedInstanceCount:   2,
		inlineWatchEnabled:      true,
		database:                database,
		appConfig:               &config.Config{},
		instanceCh:              make(chan launchInstanceRegisteredMsg, 2),
		registeredInstanceIDSet: make(map[int64]struct{}),
	}

	model, cmd := m.Update(launchInstanceRegisteredMsg{instanceID: firstID})
	got := model.(launchModel)
	if !got.inlineWatchUsed {
		t.Fatal("expected inline watch to start on first registration")
	}
	if got.inlineWatch == nil {
		t.Fatal("expected inline watch model")
	}
	if cmd == nil {
		t.Fatal("expected registration wait and watch init commands")
	}

	model, cmd = got.Update(launchInstanceRegisteredMsg{instanceID: secondID})
	got = model.(launchModel)
	if got.inlineWatch == nil {
		t.Fatal("expected inline watch model")
	}
	if cmd == nil {
		t.Fatal("expected follow-up watch command")
	}

	out := stripANSI(got.View())
	for _, want := range []string{
		fmt.Sprintf("Instance %d — RTX 3090 — launching", firstID),
		fmt.Sprintf("Instance %d — A40 — launching", secondID),
		"Bootstrap: provisioning instance",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestLaunchModelUpdate_InlineWatchKeepsRunningWithPartialFailures(t *testing.T) {
	inlineWatch := watchModel{
		instanceIDs: []int64{42},
		updates: map[int64]campaign.InstanceUpdate{
			42: {
				Launch: &db.Launch{
					ID:       42,
					Status:   db.LaunchStatusLaunching,
					Provider: "vastai",
					GPUSpec:  "A40",
				},
			},
		},
		jobProgressHWM: map[int64]int{},
	}
	model, cmd := launchModel{
		launching:       true,
		inlineWatch:     &inlineWatch,
		inlineWatchUsed: true,
	}.Update(instancesLaunchedMsg{
		instanceIDs: []int64{42},
		errors:      []error{fmt.Errorf("quota exceeded")},
	})
	got := model.(launchModel)
	if !got.done {
		t.Fatal("expected done state")
	}
	if len(got.partialErrors) != 1 {
		t.Fatalf("expected 1 partial error, got %d", len(got.partialErrors))
	}
	if cmd != nil {
		t.Fatalf("expected no quit command for inline watch partial failures, got %T", cmd)
	}

	out := stripANSI(got.View())
	if !strings.Contains(out, "1 planned launch(es) failed:") {
		t.Fatalf("expected partial failure banner, got:\n%s", out)
	}
	if !strings.Contains(out, "Instance 42 — A40 — launching") {
		t.Fatalf("expected watch view to remain visible, got:\n%s", out)
	}
}

func TestLaunchModelView_InlineWatchShowsLaunchOverviewBeforeRegistration(t *testing.T) {
	inlineWatch := watchModel{
		mode:           watchModeInstances,
		launchPending:  true,
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		initInfo:       map[int64]initialInstanceInfo{},
		jobProgressHWM: map[int64]int{},
		height:         20,
		width:          100,
	}
	m := launchModel{
		launching:     true,
		inlineWatch:   &inlineWatch,
		campaignPhase: "preparing campaign launch",
		height:        20,
		width:         100,
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "Launching instances...") {
		t.Fatalf("expected launch overview header, got:\n%s", out)
	}
	if !strings.Contains(out, "preparing campaign launch") {
		t.Fatalf("expected campaign phase in launch overview, got:\n%s", out)
	}
	if strings.Contains(out, "Unplaced Jobs") {
		t.Fatalf("did not expect unplaced jobs section before registration, got:\n%s", out)
	}
	if strings.Contains(out, "Launched") {
		t.Fatalf("did not expect watch header before registration, got:\n%s", out)
	}
}

func TestLaunchModelUpdate_InstancesLaunchedAddsReuseInstancesToInlineWatch(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusLaunching,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	inlineWatch := watchModel{
		mode:           watchModeInstances,
		launchPending:  true,
		database:       database,
		ctx:            context.Background(),
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		initInfo:       map[int64]initialInstanceInfo{},
		jobProgressHWM: map[int64]int{},
	}
	model, cmd := launchModel{
		database:    database,
		inlineWatch: &inlineWatch,
		launching:   true,
	}.Update(instancesLaunchedMsg{instanceIDs: []int64{instanceID}})
	got := model.(launchModel)
	if got.inlineWatch == nil {
		t.Fatal("expected inline watch to remain active")
	}
	if len(got.inlineWatch.instanceIDs) != 1 || got.inlineWatch.instanceIDs[0] != instanceID {
		t.Fatalf("inline watch instanceIDs = %v, want [%d]", got.inlineWatch.instanceIDs, instanceID)
	}
	if got.inlineWatch.launchPending {
		t.Fatal("expected launchPending to clear after launch completion")
	}
	if cmd == nil {
		t.Fatal("expected watch command for newly added instance")
	}
}

func TestLaunchModelAdoptTradeoffOptions_PrefersMiddleForFast(t *testing.T) {
	m := launchModel{
		launchOpts: campaign.LaunchOpts{Strategy: bidding.StrategyFast},
	}

	m.adoptTradeoffOptions([]campaign.TradeoffOption{
		{ID: "cheap", Label: "cheap"},
		{ID: "middle"},
		{ID: "fastest", Label: "fastest"},
	})

	if m.activeTradeoff != "middle" {
		t.Fatalf("activeTradeoff = %q, want middle", m.activeTradeoff)
	}
	if m.tradeoffCursor != 1 {
		t.Fatalf("tradeoffCursor = %d, want 1", m.tradeoffCursor)
	}
}

func TestLaunchModelAdoptTradeoffOptions_MapsFastestToFastWhenTwoOptions(t *testing.T) {
	m := launchModel{
		launchOpts: campaign.LaunchOpts{Strategy: bidding.StrategyFastest},
	}

	m.adoptTradeoffOptions([]campaign.TradeoffOption{
		{ID: "cheap", Label: "cheap"},
		{ID: "fast-endpoint", Label: "fast"},
	})

	if m.activeTradeoff != "fast-endpoint" {
		t.Fatalf("activeTradeoff = %q, want fast-endpoint", m.activeTradeoff)
	}
	if m.tradeoffCursor != 1 {
		t.Fatalf("tradeoffCursor = %d, want 1", m.tradeoffCursor)
	}
}

func TestLaunchModelHandleKey_RightDisclosesActiveTradeoffWithoutS(t *testing.T) {
	m := launchModel{
		focusArea:       focusJobs,
		activeTradeoff:  "fast-endpoint",
		tradeoffOptions: []campaign.TradeoffOption{{ID: "cheap"}, {ID: "fast-endpoint"}},
	}

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRight})
	got := model.(launchModel)

	if got.focusArea != focusTradeoffs {
		t.Fatalf("focusArea = %v, want focusTradeoffs", got.focusArea)
	}
	if !got.tradeoffDisclosed {
		t.Fatal("expected tradeoffDisclosed to be true")
	}
	if got.tradeoffCursor != 1 {
		t.Fatalf("tradeoffCursor = %d, want 1", got.tradeoffCursor)
	}
}

func TestLaunchModelUpdate_LaunchExecutionPlanDoesNotStartInlineWatchBeforeRegistration(t *testing.T) {
	model, cmd := launchModel{
		launching:               true,
		inlineWatchEnabled:      true,
		registeredInstanceIDSet: make(map[int64]struct{}),
	}.Update(launchExecutionPlanMsg{expectedInstanceCount: 2})

	got := model.(launchModel)
	if got.expectedInstanceCount != 2 {
		t.Fatalf("expectedInstanceCount = %d, want 2", got.expectedInstanceCount)
	}
	if got.inlineWatch != nil {
		t.Fatal("expected inline watch to remain nil until first registration")
	}
	if cmd != nil {
		t.Fatalf("expected no command, got %T", cmd)
	}
}

func TestLaunchModelView_InlineWatchHidesInventoryBeforeRegistration(t *testing.T) {
	inlineWatch := watchModel{
		mode:           watchModeInstances,
		launchPending:  true,
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		initInfo:       map[int64]initialInstanceInfo{},
		jobProgressHWM: map[int64]int{},
		height:         20,
		width:          100,
	}
	m := launchModel{
		launching:     true,
		inlineWatch:   &inlineWatch,
		campaignPhase: "launching worker instances",
		height:        20,
		width:         100,
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "Launching instances...") {
		t.Fatalf("expected launch overview header, got:\n%s", out)
	}
	if !strings.Contains(out, "launching worker instances") {
		t.Fatalf("expected campaign phase in launch overview, got:\n%s", out)
	}
	if strings.Contains(out, "Inventory Hosts") {
		t.Fatalf("did not expect inventory host table before registration, got:\n%s", out)
	}
}

func TestLaunchModelView_ShowsLaunchLivenessSummaryWhileLaunching(t *testing.T) {
	m := launchModel{
		launching:             true,
		launchStartedAt:       time.Now().Add(-65 * time.Second),
		lastLaunchProgressAt:  time.Now().Add(-30 * time.Second),
		registeredInstanceIDs: []int64{11},
		expectedInstanceCount: 3,
		launchStatusCounts: map[string]int{
			db.LaunchStatusLaunching: 1,
			db.LaunchStatusRunning:   1,
		},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"Launching instances...",
		"elapsed:",
		"new instances discovered: 1/3",
		"instance states: launching 1, running 1",
		"Still waiting for additional instance registrations",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in output, got:\n%s", want, out)
		}
	}
}

func TestLaunchModelView_FirstRegistrationWaitUsesSurvivalThresholds(t *testing.T) {
	now := time.Now()
	base := launchModel{
		launching:               true,
		launchCampaignCreatedAt: now.Add(-30 * time.Second),
		firstRegSurvival: &db.FirstRegistrationSurvival{
			WarnAfter:      2 * time.Minute,
			TerminateAfter: 5 * time.Minute,
		},
	}

	normalOut := stripANSI(base.View())
	if !strings.Contains(normalOut, "Still waiting for first instance registration") || !strings.Contains(normalOut, "within typical range") {
		t.Fatalf("expected normal first-registration wait message, got:\n%s", normalOut)
	}

	warnModel := base
	warnModel.launchCampaignCreatedAt = now.Add(-3 * time.Minute)
	warnOut := stripANSI(warnModel.View())
	if !strings.Contains(warnOut, "taking longer than typical") {
		t.Fatalf("expected warning first-registration wait message, got:\n%s", warnOut)
	}

	criticalModel := base
	criticalModel.launchCampaignCreatedAt = now.Add(-6 * time.Minute)
	criticalOut := stripANSI(criticalModel.View())
	if !strings.Contains(criticalOut, "much longer than typical") {
		t.Fatalf("expected critical first-registration wait message, got:\n%s", criticalOut)
	}
}

func TestLaunchModelUpdate_LaunchHeartbeatBackfillsInstancesAndStartsInlineWatch(t *testing.T) {
	database := db.SetupTestDB(t)
	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusLaunching})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusLaunching,
		Provider:   "vastai",
		GPUSpec:    "A40",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	model, cmd := launchModel{
		launching:               true,
		database:                database,
		appConfig:               &config.Config{},
		campaignID:              campaignID,
		inlineWatchEnabled:      true,
		registeredInstanceIDSet: make(map[int64]struct{}),
	}.Update(launchHeartbeatMsg{
		instanceIDs: []int64{instanceID},
		statusCounts: map[string]int{
			db.LaunchStatusLaunching: 1,
		},
	})
	got := model.(launchModel)
	if got.inlineWatch == nil || !got.inlineWatchUsed {
		t.Fatal("expected inline watch to start from heartbeat backfill")
	}
	if len(got.registeredInstanceIDs) != 1 || got.registeredInstanceIDs[0] != instanceID {
		t.Fatalf("registeredInstanceIDs = %v, want [%d]", got.registeredInstanceIDs, instanceID)
	}
	if cmd == nil {
		t.Fatal("expected heartbeat update to schedule follow-up command(s)")
	}
}

func TestLaunchModelUpdate_LaunchHeartbeatStatusChangeAdvancesLiveness(t *testing.T) {
	now := time.Now()
	model, _ := launchModel{
		launching:               true,
		lastLaunchProgressAt:    now.Add(-2 * time.Minute),
		registeredInstanceIDs:   []int64{11},
		registeredInstanceIDSet: map[int64]struct{}{11: {}},
		launchStatusCounts: map[string]int{
			db.LaunchStatusLaunching: 1,
		},
		lastHeartbeatStatusSig: launchStatusSignature(map[string]int{
			db.LaunchStatusLaunching: 1,
		}),
	}.Update(launchHeartbeatMsg{
		instanceIDs: []int64{11},
		statusCounts: map[string]int{
			db.LaunchStatusRunning: 1,
		},
		polledAt: now,
	})
	got := model.(launchModel)
	if !got.lastLaunchProgressAt.Equal(now) {
		t.Fatalf("lastLaunchProgressAt = %v, want %v", got.lastLaunchProgressAt, now)
	}
	if got.heartbeatProgressEvents != 1 {
		t.Fatalf("heartbeatProgressEvents = %d, want 1", got.heartbeatProgressEvents)
	}
}

func TestLaunchModelHandleKey_EnterResetsLaunchChannels(t *testing.T) {
	oldCampaignCh := make(chan campaignCreatedMsg, 1)
	oldPlanCh := make(chan launchExecutionPlanMsg, 1)
	oldPhaseCh := make(chan launchPhaseMsg, 1)
	oldInstanceCh := make(chan launchInstanceRegisteredMsg, 1)
	oldCampaignCh <- campaignCreatedMsg{campaignID: 123}
	oldPlanCh <- launchExecutionPlanMsg{expectedInstanceCount: 99}
	oldPhaseCh <- launchPhaseMsg{groupIndex: -1, phase: "stale"}
	oldInstanceCh <- launchInstanceRegisteredMsg{instanceID: 999, groupIndex: 0}

	model, cmd := launchModel{
		loading:                 false,
		selected:                map[int64]bool{1: true},
		groups:                  []campaign.InstanceGroup{{Jobs: []*db.Job{{ID: 1}}}},
		campaignCh:              oldCampaignCh,
		planCh:                  oldPlanCh,
		phaseCh:                 oldPhaseCh,
		instanceCh:              oldInstanceCh,
		registeredInstanceIDSet: make(map[int64]struct{}),
	}.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	got := model.(launchModel)

	if !got.launching {
		t.Fatal("expected launching state")
	}
	if cmd == nil {
		t.Fatal("expected launch command batch")
	}
	if got.campaignCh == oldCampaignCh || got.planCh == oldPlanCh || got.phaseCh == oldPhaseCh || got.instanceCh == oldInstanceCh {
		t.Fatal("expected launch channels to be replaced for a fresh launch")
	}
}

func TestLaunchModelHandleKey_CtrlCQuitsWhileLaunching(t *testing.T) {
	model, cmd := launchModel{launching: true}.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if model.(launchModel).launching != true {
		t.Fatal("expected model to remain in launching state until quit command executes")
	}
	assertQuitCmd(t, cmd)
}

func assertQuitCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}
