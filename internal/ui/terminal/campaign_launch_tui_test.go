package terminal

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
