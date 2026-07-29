package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// Reproduction through the autopilot's own entry point.
//
// Three fixes to machine pinning were declared complete on the strength of
// tests that called a ranking or filter function directly. Two rentals were
// spent proving those tests exercised routes production does not take. The
// reporter's prescription, adopted here: queue a pinned job, let the planner run
// its own tick, and assert the ABSENCE of wrong placement through the real
// scheduling path — rather than the presence of right placement through a
// direct call. The prior tests all passed while production misplaced.
//
// BuildAutoPlacementPlanWithOptions is what internal/orchestration/autoplanner
// calls each tick, so this is the door the failing jobs came through.
func TestAutoPlannerDoesNotPlacePinnedJobOnWrongMachine(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python probe.py", "cloud", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE jobs SET cli_overrides = ? WHERE id = ?`,
		`{"machine_affinity":["49863"]}`, jobID,
	); err != nil {
		t.Fatalf("set pin: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	// Only machines the job is NOT pinned to are on the market — the ordinary
	// case, since a pinned machine is usually rented or absent. These are the
	// two that actually took the failing jobs.
	offers := []cloud.Offer{
		{ProviderID: "a", Provider: cloud.ProviderVastai, MachineID: "140870",
			GPUName: "RTX 2060", GPUMemGB: 6, NumGPUs: 1, CostPerHour: 0.05,
			Reliability: 0.99, DiskSpaceGB: 500, CUDAVersion: 12.8},
		{ProviderID: "b", Provider: cloud.ProviderVastai, MachineID: "41452",
			GPUName: "GTX 1080 Ti", GPUMemGB: 11, NumGPUs: 1, CostPerHour: 0.10,
			Reliability: 0.99, DiskSpaceGB: 500, CUDAVersion: 12.8},
	}

	originalFetchRaw := fetchGroupRawOffersForPlanning
	t.Cleanup(func() { fetchGroupRawOffersForPlanning = originalFetchRaw })
	fetchGroupRawOffersForPlanning = func(_ *offerSearchSession, gs []InstanceGroup) []GroupRawOffers {
		out := make([]GroupRawOffers, len(gs))
		for i, g := range gs {
			out[i] = GroupRawOffers{Group: g, Offers: offers}
		}
		return out
	}

	// The planner consumes options.RawOffers; the fetch hook above is only a
	// fallback when RawOffers is empty and provider clients exist. Passing
	// pre-fetched offers is also what the autopilot does, so this matches
	// production more closely than a stubbed search.
	options := defaultPlanOptions()
	options.RawOffers = []GroupRawOffers{}
	for _, g := range GroupByAffinity([]*db.Job{job}, nil) {
		options.RawOffers = append(options.RawOffers, GroupRawOffers{Group: g, Offers: offers})
	}
	if len(options.RawOffers) == 0 {
		t.Fatal("GroupByAffinity produced no group for the pinned job")
	}

	plan, err := BuildAutoPlacementPlanWithOptions(
		database, nil, nil, []*db.Job{job}, nil, nil, nil, nil, 0, options,
	)
	if err != nil {
		t.Fatalf("BuildAutoPlacementPlanWithOptions: %v", err)
	}

	// Verified by mutation: with the job-level pin disabled in both the ranking
	// and reuse paths, this test fails reporting machine 140870 — the machine
	// that actually took wj5527. That mutation check is the only reason to trust
	// this test, since two earlier ones passed while production misplaced.

	// The assertion is about absence. A plan containing nothing for this job is
	// correct: waiting is the designed outcome when the pinned machine is not
	// available. What must never happen is a planned launch on another machine.
	for _, launch := range plan.LaunchGroups {
		for _, id := range launch.JobIDs {
			if id != jobID {
				continue
			}
			machine := "<none>"
			if launch.Offer != nil {
				machine = launch.Offer.MachineID
			}
			t.Fatalf("planner planned machine %s for a job pinned to 49863. "+
				"A pin that does not constrain placement is worse than one that is "+
				"rejected: the job completes on the wrong hardware and that reads "+
				"as success.", machine)
		}
	}

	// Reuse is the path that took wj5527, so it needs the same absence check.
	for _, assignment := range plan.ReuseAssignments {
		if assignment.Job == nil || assignment.Job.ID != jobID {
			continue
		}
		machine := "<none>"
		if assignment.Instance.Instance != nil {
			machine = assignment.Instance.Instance.MachineID
		}
		t.Fatalf("planner assigned reuse of an instance on machine %s for a job "+
			"pinned to 49863", machine)
	}
}
