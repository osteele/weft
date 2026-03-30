package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func intPtr(n int) *int { return &n }

func TestGroupByGPUSupremum(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "H100", GPUMemGB: intPtr(80)},
		{ID: 2, Status: db.StatusQueued, GPUClass: "h100", GPUMemGB: intPtr(40)},
		{ID: 3, Status: db.StatusQueued, GPUClass: "A100", GPUMemGB: intPtr(40)},
		{ID: 4, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(24)},
		{ID: 5, Status: db.StatusRunning, Host: "", GPUClass: "H100", GPUMemGB: intPtr(80)},       // effectively queued
		{ID: 6, Status: db.StatusRunning, Host: "host-a", GPUClass: "H100", GPUMemGB: intPtr(80)}, // should be excluded
	}

	groups := GroupByGPUSupremum(jobs)

	// Unconstrained job (ID=4) now forms its own group instead of merging
	// into the H100 group. We get 3 groups: H100, A100, unconstrained.
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}

	// Groups sorted by descending memory: H100 group (80GB) first
	if groups[0].GPUMemGB != 80 {
		t.Errorf("first group should have 80GB, got %d", groups[0].GPUMemGB)
	}
	if groups[0].GPUClass != "H100" {
		t.Errorf("first group should be H100, got %s", groups[0].GPUClass)
	}
	if len(groups[0].Jobs) != 3 {
		t.Errorf("H100 group should have 3 jobs, got %d", len(groups[0].Jobs))
	}

	// A100 group
	if groups[1].GPUClass != "A100" {
		t.Errorf("second group should be A100, got %s", groups[1].GPUClass)
	}
	if groups[1].GPUMemGB != 40 {
		t.Errorf("second group should have 40GB, got %d", groups[1].GPUMemGB)
	}

	// Unconstrained group
	if groups[2].GPUClass != "" {
		t.Errorf("third group should be unconstrained, got %s", groups[2].GPUClass)
	}
	if groups[2].GPUMemGB != 24 {
		t.Errorf("third group should have 24GB, got %d", groups[2].GPUMemGB)
	}
	if len(groups[2].Jobs) != 1 {
		t.Errorf("unconstrained group should have 1 job, got %d", len(groups[2].Jobs))
	}
}

func TestGroupByGPUSupremum_NvidiaAndEmptyMerge(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20)},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20)},
		{ID: 3, Status: db.StatusQueued, GPUClass: "ampere", GPUMemGB: intPtr(40)},
	}

	groups := GroupByGPUSupremum(jobs)

	// "nvidia" subsumes "ampere" → they merge. Empty-class job is separate.
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d: %v", len(groups), groups)
	}
	if groups[0].GPUClass != "AMPERE" {
		t.Errorf("constrained group should use most specific class AMPERE, got %s", groups[0].GPUClass)
	}
	if len(groups[0].Jobs) != 2 {
		t.Errorf("constrained group should have 2 jobs, got %d", len(groups[0].Jobs))
	}
	if groups[0].GPUMemGB != 40 {
		t.Errorf("constrained group should have 40GB (supremum), got %d", groups[0].GPUMemGB)
	}
	// Unconstrained group
	if groups[1].GPUClass != "" {
		t.Errorf("second group should be unconstrained, got %s", groups[1].GPUClass)
	}
	if len(groups[1].Jobs) != 1 {
		t.Errorf("unconstrained group should have 1 job, got %d", len(groups[1].Jobs))
	}
}

func TestGroupByGPUSupremum_IncompatibleGenerations(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "ampere", GPUMemGB: intPtr(40)},
		{ID: 2, Status: db.StatusQueued, GPUClass: "turing", GPUMemGB: intPtr(20)},
	}

	groups := GroupByGPUSupremum(jobs)

	// Different specific generations are incompatible
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups for incompatible generations, got %d", len(groups))
	}
}

func TestGroupByGPUSupremum_Empty(t *testing.T) {
	groups := GroupByGPUSupremum(nil)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for nil input, got %d", len(groups))
	}
}

func TestGroupByAffinity_SharedModel(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-70B"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-70B", "hf:openai/whisper-large-v3"}},
	}

	// With size func: both share Llama-3-70B → co-locate
	sizeFunc := func(modelID string) (int64, bool) {
		sizes := map[string]int64{
			"meta-llama/Llama-3-70B":  140e9,
			"openai/whisper-large-v3": 3e9,
		}
		sz, ok := sizes[modelID]
		return sz, ok
	}

	groups := GroupByAffinity(jobs, sizeFunc)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group (shared model), got %d", len(groups))
	}
	if len(groups[0].Jobs) != 2 {
		t.Errorf("group should have 2 jobs, got %d", len(groups[0].Jobs))
	}
}

func TestGroupByAffinity_NoSharedInputs(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-70B"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:openai/whisper-large-v3"}},
	}

	groups := GroupByAffinity(jobs, nil)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups (no shared inputs), got %d", len(groups))
	}
}

func TestGroupByAffinity_NilSizeFuncUsesRefCount(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-70B"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-70B"}},
	}

	// Even without a size func, shared ref count triggers co-location
	groups := GroupByAffinity(jobs, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group (ref-count affinity), got %d", len(groups))
	}
}

func TestGroupByAffinity_MixedConstrainedUnconstrained(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "A10", GPUMemGB: intPtr(24),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
		{ID: 3, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:openai/whisper-large-v3"}},
	}

	groups := GroupByAffinity(jobs, nil)

	// 3 groups: A10 constrained, unconstrained with Llama, unconstrained with whisper
	// Job 2 does NOT merge into A10 group despite sharing the same model.
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}

	// A10 group should have only job 1
	a10Found := false
	for _, g := range groups {
		if g.GPUClass == "A10" {
			a10Found = true
			if len(g.Jobs) != 1 {
				t.Errorf("A10 group should have 1 job, got %d", len(g.Jobs))
			}
		}
	}
	if !a10Found {
		t.Error("expected an A10 group")
	}
}

func TestGroupByAffinity_UnconstrainedNoInputs(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20)},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20)},
	}

	groups := GroupByAffinity(jobs, nil)

	// No inputs → no affinity → separate groups
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups (no inputs), got %d", len(groups))
	}
}

func TestGroupByAffinity_ObservedInputsCount(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			ObservedInputs: []string{"hf:meta-llama/Llama-3-8B"}},
	}

	groups := GroupByAffinity(jobs, nil)

	// ObservedInputs should also contribute to affinity
	if len(groups) != 1 {
		t.Fatalf("expected 1 group (observed input affinity), got %d", len(groups))
	}
}

func TestInstanceGroupHasComputeIntensiveJob(t *testing.T) {
	group := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 1, Tags: []string{"rental"}},
			{ID: 2, Tags: []string{"compute-intensive", "rental"}},
		},
	}
	if !group.HasComputeIntensiveJob() {
		t.Error("expected HasComputeIntensiveJob() = true")
	}

	groupNo := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 3, Tags: []string{"rental"}},
		},
	}
	if groupNo.HasComputeIntensiveJob() {
		t.Error("expected HasComputeIntensiveJob() = false")
	}

	empty := InstanceGroup{}
	if empty.HasComputeIntensiveJob() {
		t.Error("expected HasComputeIntensiveJob() = false for empty group")
	}
}

func TestInstanceGroupGPUSpec(t *testing.T) {
	tests := []struct {
		group InstanceGroup
		want  string
	}{
		{InstanceGroup{GPUClass: "H100", GPUMemGB: 80}, "H100 ≥80GB"},
		{InstanceGroup{GPUClass: "A100"}, "A100"},
		{InstanceGroup{GPUMemGB: 24}, "≥24GB"},
		{InstanceGroup{}, "GPU"},
	}
	for _, tt := range tests {
		got := tt.group.GPUSpec()
		if got != tt.want {
			t.Errorf("GPUSpec() = %q, want %q", got, tt.want)
		}
	}
}
