package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func jobPinnedTo(id int64, machines ...string) *db.Job {
	return &db.Job{ID: id, CLIResourceOverrides: &db.CLIResourceOverrides{MachineAffinity: machines}}
}

// A job carries its own pin so "run this next to that earlier run" survives
// from submit until placement, which may be many autopilot ticks later.
func TestEffectiveMachineAffinity_PerJobOnly(t *testing.T) {
	got := effectiveMachineAffinity(nil, []*db.Job{jobPinnedTo(1, "vastai/49863")})
	if _, ok := got["vastai/49863"]; !ok || len(got) != 1 {
		t.Fatalf("effectiveMachineAffinity = %v, want the job's own pin", got)
	}
}

func TestEffectiveMachineAffinity_LaunchLevelOnly(t *testing.T) {
	launch := map[string]struct{}{"vastai/49863": {}}
	got := effectiveMachineAffinity(launch, []*db.Job{{ID: 1}})
	if len(got) != 1 {
		t.Fatalf("effectiveMachineAffinity = %v, want the launch pin unchanged", got)
	}
}

// Both present narrows. A launch-level --affinity restricts the machines under
// consideration; a job's own pin restricts further. Union would let a job widen
// a launch pin, which is the opposite of what either flag means.
func TestEffectiveMachineAffinity_IntersectsRatherThanUnions(t *testing.T) {
	launch := map[string]struct{}{"vastai/49863": {}, "vastai/11111": {}}
	got := effectiveMachineAffinity(launch, []*db.Job{jobPinnedTo(1, "vastai/49863")})
	if len(got) != 1 {
		t.Fatalf("effectiveMachineAffinity = %v, want only the machine both accept", got)
	}
	if _, ok := got["vastai/49863"]; !ok {
		t.Errorf("effectiveMachineAffinity = %v, want vastai/49863", got)
	}
}

// Disjoint pins cannot both be satisfied. Falling back to either side would
// silently honour one flag and drop the other; an unsatisfiable group is the
// honest outcome and surfaces as ErrMachineAffinityUnsatisfied.
func TestEffectiveMachineAffinity_DisjointIsUnsatisfiable(t *testing.T) {
	launch := map[string]struct{}{"vastai/11111": {}}
	got := effectiveMachineAffinity(launch, []*db.Job{jobPinnedTo(1, "vastai/49863")})
	if _, ok := got["vastai/11111"]; ok {
		t.Error("a disjoint pin must not fall back to the launch-level machine")
	}
	if _, ok := got["vastai/49863"]; ok {
		t.Error("a disjoint pin must not fall back to the job's machine")
	}
	if len(got) == 0 {
		t.Error("want a sentinel that matches no offer, not an empty set meaning unconstrained")
	}
}

// Across a group, pins union: each job names machines it accepts, so the group
// can run where any member accepts. Intersecting here would make two
// differently-pinned jobs unplaceable together when the right outcome is to
// place them separately — a grouping decision, not an eligibility one.
func TestRequestedMachineAffinityForJobs_UnionsAcrossJobs(t *testing.T) {
	got := db.RequestedMachineAffinityForJobs([]*db.Job{
		jobPinnedTo(1, "vastai/49863"),
		jobPinnedTo(2, "vastai/11111"),
		{ID: 3},
	})
	if len(got) != 2 {
		t.Fatalf("RequestedMachineAffinityForJobs = %v, want both pins", got)
	}
}

// A raw machine id is stored as the user typed it — ResolveMachineIDs keys
// wi/wj tokens but keeps a bare id as provided — while the offer filter
// compares provider-qualified keys. Unkeyed, `--affinity 49863` matched no
// offer, which on the planning path meant "no constraint" rather than "no
// placement".
func TestEffectiveMachineAffinity_KeysRawMachineIDs(t *testing.T) {
	got := effectiveMachineAffinity(nil, []*db.Job{jobPinnedTo(1, "49863")})
	if _, ok := got["vastai/49863"]; !ok {
		t.Fatalf("effectiveMachineAffinity = %v, want a provider-qualified key", got)
	}
}

// The test that was missing. The original suite asserted a pinned job CAN land
// on a matching machine; it never asserted it CANNOT land elsewhere. The bug
// passed the first and failed the second: wj5526 was pinned to 49863 and ran
// on 41452, producing a GTX 1080 Ti measurement attributed to the wrong box.
//
// A pin that is accepted, persisted, and ignored is worse than one that is
// rejected: a pinned job is expected to sit unplaced for a long time, so
// "still queued" and "working" look identical, and completing quickly reads as
// good news.
func TestPinnedJobDoesNotLandOnNonMatchingMachine(t *testing.T) {
	pinned := jobPinnedTo(1, "49863")
	offers := []cloud.Offer{
		{ProviderID: "a", Provider: cloud.ProviderVastai, MachineID: "41452", GPUName: "GTX 1080 Ti", GPUMemGB: 11, CostPerHour: 0.10},
		{ProviderID: "b", Provider: cloud.ProviderVastai, MachineID: "99999", GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.20},
	}
	affinity := effectiveMachineAffinity(nil, []*db.Job{pinned})
	kept := filterOffersByMachineAffinity(offers, affinity)
	if len(kept) != 0 {
		t.Fatalf("kept %d offer(s) for a machine the job is not pinned to: %+v", len(kept), kept)
	}
}

// And the positive case still holds, so the fix is not simply rejecting
// everything.
func TestPinnedJobLandsOnMatchingMachine(t *testing.T) {
	pinned := jobPinnedTo(1, "49863")
	offers := []cloud.Offer{
		{ProviderID: "a", Provider: cloud.ProviderVastai, MachineID: "41452", GPUName: "GTX 1080 Ti"},
		{ProviderID: "b", Provider: cloud.ProviderVastai, MachineID: "49863", GPUName: "RTX 5880 Ada"},
	}
	kept := filterOffersByMachineAffinity(offers, effectiveMachineAffinity(nil, []*db.Job{pinned}))
	if len(kept) != 1 || kept[0].MachineID != "49863" {
		t.Fatalf("kept = %+v, want only the pinned machine", kept)
	}
}
