package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// Reproduction at the planner level, not the ranker level: the ranker-level
// tests passed while production placed a pinned job on the wrong machine,
// because the planner reaches ranking through a path they never exercised.
// This drives buildProfilePlansFromSplitRawWithSession, which is what the
// autopilot tick runs.
func TestPlannerHonoursMachineAffinity(t *testing.T) {
	const pin = "49863"

	// Deliberately the most expensive offer on the market. The cheap profile
	// would take either decoy, so selecting this one can only be the pin.
	pinnedOffer := cloud.Offer{
		ProviderID: "pinned", Provider: cloud.ProviderVastai, MachineID: pin,
		GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.90, DLPerf: 40, Reliability: 0.99,
	}
	decoys := []cloud.Offer{
		{ProviderID: "a", Provider: cloud.ProviderVastai, MachineID: "140870",
			GPUName: "RTX 2060", GPUMemGB: 6, CostPerHour: 0.05, DLPerf: 10, Reliability: 0.99},
		{ProviderID: "b", Provider: cloud.ProviderVastai, MachineID: "41452",
			GPUName: "GTX 1080 Ti", GPUMemGB: 11, CostPerHour: 0.10, DLPerf: 12, Reliability: 0.99},
	}

	t.Run("selects the pinned machine over cheaper alternatives", func(t *testing.T) {
		selected := plannerOffersFor(t, append([]cloud.Offer{pinnedOffer}, decoys...), pin)
		if len(selected) == 0 {
			t.Fatal("planner placed nothing when an offer on the pinned machine was available")
		}
		for _, offer := range selected {
			if offer.MachineID != pin {
				t.Errorf("planner selected machine %s (%s) for a job pinned to %s",
					offer.MachineID, offer.GPUName, pin)
			}
		}
	})

	// The real situation, since a pinned machine is usually rented or absent.
	// A pin that displays correctly and does not constrain placement is worse
	// than one that is rejected: the job completes quickly on the wrong
	// hardware and that reads as success.
	t.Run("places nothing when the pinned machine is off the market", func(t *testing.T) {
		for _, offer := range plannerOffersFor(t, decoys, pin) {
			t.Errorf("planner selected machine %s (%s) for a job pinned to %s",
				offer.MachineID, offer.GPUName, pin)
		}
	})
}

// plannerOffersFor runs the planner for a single job pinned to machineID
// against the given market, and returns the offers it chose.
func plannerOffersFor(t *testing.T, market []cloud.Offer, machineID string) []*cloud.Offer {
	t.Helper()

	job := &db.Job{
		ID:      1,
		Command: "python probe.py",
		CLIResourceOverrides: &db.CLIResourceOverrides{
			MachineAffinity: []string{machineID},
		},
	}
	groups := []InstanceGroup{{GPUClass: "NVIDIA", Jobs: []*db.Job{job}}}
	splitRaw := []GroupRawOffers{{Group: groups[0], Offers: market}}

	// CandidatePlanModeFull builds merged and parallel candidates through this
	// hook; pin it to the split candidate so the market under test is the only
	// input and no live client is consulted.
	original := fetchCandidateGroupingsForPlanning
	t.Cleanup(func() { fetchCandidateGroupingsForPlanning = original })
	fetchCandidateGroupingsForPlanning = func(_ *offerSearchSession, splitGroups []InstanceGroup, _ func(InstanceGroup) int) []GroupingCandidate {
		return []GroupingCandidate{{Label: "split", Groups: splitGroups, Raw: splitRaw}}
	}

	plans := buildProfilePlansFromSplitRawWithSession(
		nil, groups, splitRaw, nil, nil, nil, nil,
		newOfferSearchSession(nil, 0.95),
		[]ProfilePlanSpec{{Profile: bidding.StrategyCheap.Profile(), CandidateMode: CandidatePlanModeFull}},
		0, nil, defaultPlanOptions(),
	)

	plan, ok := plans[bidding.StrategyCheap.Profile().ID]
	if !ok {
		t.Fatalf("no plan produced: %#v", plans)
	}
	if plan.NewCandidate == nil {
		return nil
	}
	var selected []*cloud.Offer
	for _, groupOffer := range plan.NewCandidate.Offers {
		if groupOffer.Offer != nil {
			selected = append(selected, groupOffer.Offer)
		}
	}
	return selected
}
