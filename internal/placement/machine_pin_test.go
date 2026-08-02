package placement

import (
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
)

// A pin names a provider physical machine. Targets are judged by their
// MachineKey: a matching key passes, a different key fails, and no key fails
// closed — which is what makes every on-prem host ineligible for pinned jobs,
// since on-prem hosts have no provider machine identity.
func TestEvaluateEligibility_MachinePin(t *testing.T) {
	cases := []struct {
		name     string
		pins     []string
		key      string
		eligible bool
	}{
		{"unpinned ignores machine identity", nil, "", true},
		{"bare pin matches its vastai machine", []string{"49863"}, "vastai/49863", true},
		{"qualified pin matches", []string{"vastai/49863"}, "vastai/49863", true},
		{"pin rejects another machine", []string{"49863"}, "vastai/140870", false},
		{"pin rejects a target with no machine identity", []string{"49863"}, "", false},
		{"any pin in the set suffices", []string{"vastai/11111", "49863"}, "vastai/49863", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := EvaluateEligibility(
				Constraints{MachineAffinity: tc.pins},
				TargetSpec{Name: "target", MachineKey: tc.key},
			)
			if verdict.Eligible != tc.eligible {
				t.Fatalf("Eligible = %v, want %v (reasons: %v)", verdict.Eligible, tc.eligible, verdict.Messages())
			}
			if !tc.eligible && verdict.Reasons[0].Kind != ReasonMachinePin {
				t.Errorf("reason kind = %q, want %q", verdict.Reasons[0].Kind, ReasonMachinePin)
			}
		})
	}
}

// The pin travels from CLIResourceOverrides into resolved constraints; a pin
// that stops at the overrides struct is invisible to every consumer of
// ConstraintsFromJob, which is how the on-prem path ignored pins.
func TestConstraintsFromJob_CarriesMachineAffinity(t *testing.T) {
	job := &db.Job{
		ID:                   7,
		CLIResourceOverrides: &db.CLIResourceOverrides{MachineAffinity: []string{"49863"}},
	}
	c := ConstraintsFromJob(job)
	if len(c.MachineAffinity) != 1 || c.MachineAffinity[0] != "49863" {
		t.Fatalf("MachineAffinity = %v, want the job's pin", c.MachineAffinity)
	}
}

// Entry-point reproduction through the real scorer: the same host that takes
// an unpinned job must refuse a pinned one. A pinned job placed on-prem
// completes quickly on hardware nobody asked for, which reads as success —
// the same invisible failure the cloud-side pin fixes addressed.
func TestSelectHostFromSnapshot_SkipsPinnedJob(t *testing.T) {
	database := db.SetupTestDB(t)
	hosts := []inventory.HostSpec{{
		Name: "host-alpha",
		GPUs: []inventory.GPUSpec{{Name: "RTX 3090", Class: "3090", Memory: "24GB"}},
	}}
	metrics := map[string]*HostMetrics{"host-alpha": {QueueDepth: 0}}

	unpinned := &db.Job{ID: 1, GPUClass: "nvidia"}
	host, ok := SelectHostFromSnapshot(database, hosts, metrics, unpinned, nil, nil)
	if !ok || host != "host-alpha" {
		t.Fatalf("unpinned job: SelectHostFromSnapshot = (%q, %v), want host-alpha — the host must be otherwise eligible for this test to bind on the pin", host, ok)
	}

	pinned := &db.Job{
		ID:                   2,
		GPUClass:             "nvidia",
		CLIResourceOverrides: &db.CLIResourceOverrides{MachineAffinity: []string{"49863"}},
	}
	if host, ok := SelectHostFromSnapshot(database, hosts, metrics, pinned, nil, nil); ok {
		t.Fatalf("pinned job placed on-prem on %q; a pin names a provider machine no on-prem host is", host)
	}
}

// On-prem hosts are judged on the same host axes the cloud paths enforce:
// an explicit --cpu-cores floor against inventory cpu_cores (unknown does not
// disqualify, matching the RAM floor), and --interconnect against GPU naming
// through the shared InterconnectSatisfied predicate.
func TestScoreHost_HostAxes(t *testing.T) {
	database := db.SetupTestDB(t)
	host := func(cores int, gpuName string) inventory.HostSpec {
		return inventory.HostSpec{
			Name:     "host-alpha",
			CPUCores: cores,
			GPUs:     []inventory.GPUSpec{{Name: gpuName, Class: "nvidia", Memory: "80GB"}},
		}
	}
	metrics := map[string]*HostMetrics{"host-alpha": {QueueDepth: 0}}
	cases := []struct {
		name        string
		constraints Constraints
		host        inventory.HostSpec
		want        bool
	}{
		{"cores floor rejects insufficient", Constraints{CPUCores: 32}, host(16, "A100-SXM4-80GB"), false},
		{"cores floor passes unknown", Constraints{CPUCores: 32}, host(0, "A100-SXM4-80GB"), true},
		{"cores floor passes sufficient", Constraints{CPUCores: 32}, host(64, "A100-SXM4-80GB"), true},
		{"nvlink rejects plain GPU naming", Constraints{Interconnect: "nvlink", GPUClass: "nvidia"}, host(64, "RTX 3090"), false},
		{"nvlink accepts SXM naming", Constraints{Interconnect: "nvlink", GPUClass: "nvidia"}, host(64, "A100-SXM4-80GB"), true},
		{"pcie rejects SXM naming", Constraints{Interconnect: "pcie", GPUClass: "nvidia"}, host(64, "A100-SXM4-80GB"), false},
		{"no interconnect requirement passes", Constraints{GPUClass: "nvidia"}, host(64, "RTX 3090"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scores := ScoreHostListWithMetrics(database, []inventory.HostSpec{tc.host}, tc.constraints, metrics)
			if len(scores) != 1 {
				t.Fatalf("scores = %d, want 1", len(scores))
			}
			if scores[0].Eligible != tc.want {
				t.Errorf("Eligible = %v, want %v (reasons: %v)", scores[0].Eligible, tc.want, scores[0].Reasons)
			}
		})
	}
}

// A provider request is a routing constraint: no on-prem host (no provider)
// and no other provider's target can satisfy it. Checked in the shared
// eligibility predicate so the launch prefilter cannot on-prem-place a
// --provider job the autopilot passes would have skipped.
func TestEvaluateEligibility_Provider(t *testing.T) {
	cases := []struct {
		name     string
		req      string
		target   string
		eligible bool
	}{
		{"no request ignores provider", "", "", true},
		{"request rejects providerless target", "vastai", "", false},
		{"request rejects another provider", "vastai", "runpod", false},
		{"request matches case-insensitively", "vastai", "Vastai", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := EvaluateEligibility(
				Constraints{Provider: tc.req},
				TargetSpec{Name: "target", Provider: tc.target},
			)
			if verdict.Eligible != tc.eligible {
				t.Fatalf("Eligible = %v, want %v (reasons: %v)", verdict.Eligible, tc.eligible, verdict.Messages())
			}
			if !tc.eligible && verdict.Reasons[0].Kind != ReasonProvider {
				t.Errorf("reason kind = %q, want %q", verdict.Reasons[0].Kind, ReasonProvider)
			}
		})
	}
}

func TestSelectHostFromSnapshot_SkipsProviderRequestedJob(t *testing.T) {
	database := db.SetupTestDB(t)
	hosts := []inventory.HostSpec{{
		Name: "host-alpha",
		GPUs: []inventory.GPUSpec{{Name: "RTX 3090", Class: "3090", Memory: "24GB"}},
	}}
	metrics := map[string]*HostMetrics{"host-alpha": {QueueDepth: 0}}

	job := &db.Job{ID: 3, GPUClass: "nvidia", Tags: []string{"provider:vastai"}}
	if host, ok := SelectHostFromSnapshot(database, hosts, metrics, job, nil, nil); ok {
		t.Fatalf("provider-requesting job placed on-prem on %q", host)
	}
}
