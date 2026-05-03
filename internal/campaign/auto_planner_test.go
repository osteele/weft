package campaign

import (
	"errors"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestApplyGroupOffer_LaunchableSkipsOnlyReusedJobs(t *testing.T) {
	plan := AutoPlacementPlan{BlockedReasons: map[int64]string{}}
	group := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 706},
			{ID: 707},
		},
	}
	offer := GroupOffer{Offer: &cloud.Offer{}}
	reused := map[int64]struct{}{706: {}}

	applyGroupOffer(&plan, group, offer, reused, 0.95)

	if len(plan.BlockedReasons) != 0 {
		t.Fatalf("blocked reasons = %v, want none", plan.BlockedReasons)
	}
}

func TestApplyGroupOffer_BlocksOnlyUnreusedJobsWhenNoOffer(t *testing.T) {
	plan := AutoPlacementPlan{BlockedReasons: map[int64]string{}}
	group := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 706},
			{ID: 707},
		},
	}
	offer := GroupOffer{Err: errors.New("capacity unavailable")}
	reused := map[int64]struct{}{706: {}}

	applyGroupOffer(&plan, group, offer, reused, 0.95)

	if got := plan.BlockedReasons[707]; got != "planner: capacity unavailable" {
		t.Fatalf("blocked reason for 707 = %q, want %q", got, "planner: capacity unavailable")
	}
	if _, exists := plan.BlockedReasons[706]; exists {
		t.Fatalf("unexpected blocked reason for reused job 706: %q", plan.BlockedReasons[706])
	}
}

func TestApplyGroupOffer_NoOfferEmitsConstraintAwareReason(t *testing.T) {
	plan := AutoPlacementPlan{BlockedReasons: map[int64]string{}}
	group := InstanceGroup{
		GPUClass: "ampere+",
		GPUMemGB: 40,
		Jobs:     []*db.Job{{ID: 910}},
	}
	offer := GroupOffer{
		FilterStats: OfferFilterStats{RawCount: 12, AfterVRAM: 0},
	}

	applyGroupOffer(&plan, group, offer, nil, 0.95)

	got := plan.BlockedReasons[910]
	if got == "planner: no compatible offers" {
		t.Fatalf("blocked reason regressed to generic message: %q", got)
	}
	if !strings.Contains(got, "12 offers found") || !strings.Contains(got, "VRAM") {
		t.Fatalf("blocked reason = %q, want a constraint-aware message mentioning offer count and VRAM", got)
	}
}

func TestBuildLaunchGroups_SkipsReusedJobsAndCostsPerGroup(t *testing.T) {
	candidate := &CandidateResult{
		Groups: []InstanceGroup{
			{Jobs: []*db.Job{{ID: 101}, {ID: 102}}},
			{Jobs: []*db.Job{{ID: 103}}},
		},
		Offers: []GroupOffer{
			{Offer: &cloud.Offer{CostPerHour: 0.99}},
			{Offer: &cloud.Offer{CostPerHour: 0.50}},
		},
	}
	reused := map[int64]struct{}{101: {}}

	groups := buildLaunchGroups(candidate, reused)
	if len(groups) != 2 {
		t.Fatalf("launch groups len = %d, want 2", len(groups))
	}
	if got, want := groups[0].JobIDs, []int64{102}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("group 0 job ids = %v, want %v", got, want)
	}
	if groups[0].CostPerHourCents != 99 {
		t.Fatalf("group 0 cost = %d, want 99", groups[0].CostPerHourCents)
	}
	if groups[1].CostPerHourCents != 50 {
		t.Fatalf("group 1 cost = %d, want 50", groups[1].CostPerHourCents)
	}
}

func TestBuildLaunchGroups_OrdersPriorityJobsFirst(t *testing.T) {
	candidate := &CandidateResult{
		Groups: []InstanceGroup{
			{Jobs: []*db.Job{{ID: 101}, {ID: 102, Priority: 1}}},
		},
		Offers: []GroupOffer{{Offer: &cloud.Offer{CostPerHour: 0.99}}},
	}

	groups := buildLaunchGroups(candidate, nil)
	if len(groups) != 1 {
		t.Fatalf("launch groups len = %d, want 1", len(groups))
	}
	if got, want := groups[0].JobIDs, []int64{102, 101}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("group job ids = %v, want %v", got, want)
	}
	if groups[0].Priority != 1 {
		t.Fatalf("group priority = %d, want 1", groups[0].Priority)
	}
}

func TestApplyGroupOffer_MappedStatsDoNotRegressToProviderEmpty(t *testing.T) {
	split := []InstanceGroup{
		{
			GPUClass: "RTX-5090",
			GPUMemGB: 20,
			Jobs:     []*db.Job{{ID: 1229}},
		},
	}
	result := CandidateResult{
		Groups: split,
		Offers: []GroupOffer{
			{
				Group: split[0],
				// Simulate: provider search found offers, but they were filtered later.
				FilterStats: OfferFilterStats{
					RawCount:         20,
					AfterVRAM:        20,
					AfterCUDA:        0,
					CUDAImage:        "nvidia/cuda:12.4.1-runtime-ubuntu22.04",
					CUDAImageVersion: 12.4,
					CUDAMinRequired:  12.8,
					CUDAExampleGPU:   "RTX 5090",
				},
			},
		},
	}
	mapped := MapOffersToSplitGroups(split, result)
	if len(mapped) != 1 {
		t.Fatalf("mapped len = %d, want 1", len(mapped))
	}

	plan := AutoPlacementPlan{BlockedReasons: map[int64]string{}}
	applyGroupOffer(&plan, split[0], mapped[0], nil, 0.95)

	reason := plan.BlockedReasons[1229]
	if strings.Contains(reason, "no offers from providers") {
		t.Fatalf("blocked reason regressed to provider-empty despite RawCount>0: %q", reason)
	}
	if !strings.Contains(reason, "CUDA compatibility") {
		t.Fatalf("blocked reason should mention CUDA compatibility, got: %q", reason)
	}
}
