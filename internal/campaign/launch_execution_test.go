package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// TestPrepareNewInstanceLaunchPlan_MergesCompatibleSplitGroupsWithDisjointHF
// is the regression test for the move-to-new grouping bug: floatable jobs
// with disjoint HF inputs but otherwise-identical compatibility used to be
// hard-split because affinityGroupUnconstrained's `bestScore > 0` gate
// (campaign.go) treated overlap as a binning key. Routing move-to-new
// through the planner means MergeCompatibleGroups can rejoin them when
// the merged candidate scores at least as well as the split candidate.
//
// Spec: AssetOverlapLaunchGrouping in
// specs/campaign-lifecycle.allium ("only a scoring benefit; it must not
// merge incompatible groups" — and, symmetrically, the absence of overlap
// must not block merging of compatible ones).
func TestPrepareNewInstanceLaunchPlan_MergesCompatibleSplitGroupsWithDisjointHF(t *testing.T) {
	originalFetchCandidates := fetchCandidateGroupingsForPlanning
	t.Cleanup(func() {
		fetchCandidateGroupingsForPlanning = originalFetchCandidates
	})

	// The cheap profile drives CandidatePlanModeFull, which builds split /
	// merged / parallel candidates via fetchCandidateGroupingsForPlanning.
	// Override the hook so the merged candidate has a single cheap offer
	// (one $0.50/hr instance running two jobs sequentially is cheaper than
	// two $1.00/hr instances running in parallel), forcing the planner to
	// pick "merged" over "split". The split candidate's per-group offer
	// comes from the MockClient below.
	fetchCandidateGroupingsForPlanning = func(_ *offerSearchSession, splitGroups []InstanceGroup) []GroupingCandidate {
		// Reproduce the production candidate set (split + merged + parallel)
		// but with controlled raw offers.
		mergedGroups := MergeCompatibleGroups(splitGroups)
		makeRaw := func(groups []InstanceGroup, cost float64, providerID string) []GroupRawOffers {
			raw := make([]GroupRawOffers, len(groups))
			for i, g := range groups {
				raw[i] = GroupRawOffers{
					Group: g,
					Offers: []cloud.Offer{{
						ProviderID: providerID, GPUName: "A100", GPUMemGB: 40, CostPerHour: cost, DLPerf: 40, Reliability: 0.99,
					}},
				}
			}
			return raw
		}
		candidates := []GroupingCandidate{
			{Label: "split", Groups: splitGroups, Raw: makeRaw(splitGroups, 1.00, "split-alt")},
		}
		if len(mergedGroups) != len(splitGroups) {
			candidates = append(candidates, GroupingCandidate{
				Label: "merged", Groups: mergedGroups, Raw: makeRaw(mergedGroups, 0.50, "merged"),
			})
		}
		return candidates
	}

	mockClient := &cloud.MockClient{
		SearchOffersFunc: func(_ cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{{
				ProviderID: "split", GPUName: "A100", GPUMemGB: 40, CostPerHour: 1.00, DLPerf: 40, Reliability: 0.99,
			}}, nil
		},
	}

	// Two split groups with identical GPU class / VRAM tier but disjoint
	// HF inputs — the historical hard gate would have kept them split.
	splitGroups := []InstanceGroup{
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 40,
			Jobs: []*db.Job{{
				ID:       1,
				Status:   db.StatusQueued,
				GPUClass: "nvidia",
				GPUMemGB: intPtr(40),
				Inputs:   []string{"hf:project-a/model"},
				Command:  "python a.py",
			}},
		},
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 40,
			Jobs: []*db.Job{{
				ID:       2,
				Status:   db.StatusQueued,
				GPUClass: "nvidia",
				GPUMemGB: intPtr(40),
				Inputs:   []string{"hf:project-b/model"},
				Command:  "python b.py",
			}},
		},
	}

	prep, err := PrepareNewInstanceLaunchPlan(
		nil,
		[]cloud.Client{mockClient},
		nil,
		splitGroups,
		nil,
		bidding.StrategyCheap.Profile(),
		0,
		0.95,
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("PrepareNewInstanceLaunchPlan: %v", err)
	}
	if prep.StrategyPlan.NewCandidate == nil {
		t.Fatalf("StrategyPlan.NewCandidate = nil, want a candidate")
	}
	if got := prep.StrategyPlan.NewCandidate.Label; got != "merged" {
		t.Fatalf("NewCandidate.Label = %q, want \"merged\" (disjoint-HF jobs must merge when compatible and cheaper)", got)
	}
	if len(prep.LaunchGroups) != 1 {
		t.Fatalf("LaunchGroups = %d, want 1 (single merged instance)", len(prep.LaunchGroups))
	}
	if got := len(prep.LaunchGroups[0].Jobs); got != 2 {
		t.Fatalf("merged group jobs = %d, want 2", got)
	}
}

// TestPrepareNewInstanceLaunchPlan_SkipsReuseEvaluation guards the
// `--to new` contract: even with a populated database whose reusable-
// instance set covers the job's requirements, the function must not
// produce a reuse assignment. Uses a real test DB seeded with a
// running A100 launch so that a future refactor calling
// FindReusableInstances(database) would surface a candidate and break
// the test — sharper guard than passing a nil DB, which would silently
// satisfy any "no reuse" assertion.
func TestPrepareNewInstanceLaunchPlan_SkipsReuseEvaluation(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUClass:        "NVIDIA",
		ResolvedGPUName: "A100",
		GPUMemGB:        40,
		DiskGB:          200,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	t.Logf("seeded reusable launch id=%d", launchID)

	mockClient := &cloud.MockClient{
		SearchOffersFunc: func(_ cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{{
				ProviderID: "fresh", GPUName: "A100", GPUMemGB: 40, CostPerHour: 1.00, DLPerf: 40, Reliability: 0.99,
			}}, nil
		},
	}
	groups := []InstanceGroup{{
		GPUClass: "NVIDIA",
		GPUMemGB: 40,
		Jobs: []*db.Job{{
			ID:       1,
			Status:   db.StatusQueued,
			GPUClass: "nvidia",
			GPUMemGB: intPtr(40),
			Command:  "python a.py",
		}},
	}}

	prep, err := PrepareNewInstanceLaunchPlan(
		database,
		[]cloud.Client{mockClient},
		nil,
		groups,
		nil,
		bidding.StrategyCheap.Profile(),
		0,
		0.95,
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("PrepareNewInstanceLaunchPlan: %v", err)
	}
	if len(prep.StrategyPlan.ReuseAssignments) != 0 {
		t.Fatalf("ReuseAssignments = %d, want 0 (move-to-new must not reuse instances)", len(prep.StrategyPlan.ReuseAssignments))
	}
	if prep.StrategyPlan.NewCandidate == nil {
		t.Fatalf("NewCandidate = nil, want a fresh-instance plan")
	}
}
