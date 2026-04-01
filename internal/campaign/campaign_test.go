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

func TestGroupByGPUSupremum_FloatableGoToAffinityPath(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20)},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20)},
		{ID: 3, Status: db.StatusQueued, GPUClass: "ampere", GPUMemGB: intPtr(40)},
	}

	groups := GroupByGPUSupremum(jobs)

	// All three are floatable/unconstrained and have no shared inputs,
	// so they form 3 separate groups (no affinity to merge on).
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d: %v", len(groups), groups)
	}
}

func TestGroupByAffinity_NvidiaFloatsWithSharedInputs(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(24),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
	}

	groups := GroupByAffinity(jobs, nil)

	// Both nvidia jobs share a model → co-located in one group with GPUClass "NVIDIA"
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].GPUClass != "NVIDIA" {
		t.Errorf("group should have GPUClass NVIDIA, got %s", groups[0].GPUClass)
	}
	if groups[0].GPUMemGB != 24 {
		t.Errorf("group should have 24GB (supremum), got %d", groups[0].GPUMemGB)
	}
	if len(groups[0].Jobs) != 2 {
		t.Errorf("group should have 2 jobs, got %d", len(groups[0].Jobs))
	}
}

func TestGroupByAffinity_NvidiaNotMergedWithPinnedModel(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "3090", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
	}

	groups := GroupByAffinity(jobs, nil)

	// nvidia (floatable) and 3090 (pinned) go through different paths and
	// should NOT be merged, even though they share inputs.
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
}

func TestGroupByAffinity_NvidiaAndEmptyMergeWithSharedInputs(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
	}

	groups := GroupByAffinity(jobs, nil)

	// nvidia and empty are both floatable and share a model → merged.
	// Group gets GPUClass "NVIDIA" (the narrower constraint).
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].GPUClass != "NVIDIA" {
		t.Errorf("group should have GPUClass NVIDIA, got %s", groups[0].GPUClass)
	}
}

func TestGroupByAffinity_IncompatibleFloatableConstraints(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "ampere", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "hopper", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
	}

	groups := GroupByAffinity(jobs, nil)

	// ampere and hopper are incompatible generations → separate groups
	// despite sharing inputs.
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
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
		{InstanceGroup{GPUClass: "A100", GPUMemGB: 24, MaxGPUMemGB: 48}, "A100 ≥24GB ≤48GB"},
		{InstanceGroup{GPUMemGB: 24, MaxGPUMemGB: 24}, "≥24GB ≤24GB"},
	}
	for _, tt := range tests {
		got := tt.group.GPUSpec()
		if got != tt.want {
			t.Errorf("GPUSpec() = %q, want %q", got, tt.want)
		}
	}
}

func TestGroupByAffinityPropagatesMaxGPUMemGB(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			GPUMemMaxGB: intPtr(24),
			Inputs:      []string{"hf:model-a"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			GPUMemMaxGB: intPtr(48),
			Inputs:      []string{"hf:model-a"}},
	}
	groups := GroupByAffinity(jobs, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	// Both jobs have ceilings: group ceiling = max(24, 48) = 48
	if groups[0].MaxGPUMemGB != 48 {
		t.Errorf("MaxGPUMemGB = %d, want 48", groups[0].MaxGPUMemGB)
	}
}

func TestGroupCeilingDropsWhenAnyJobUncapped(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			GPUMemMaxGB: intPtr(24),
			Inputs:      []string{"hf:model-a"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:model-a"}}, // no ceiling
	}
	groups := GroupByAffinity(jobs, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	// Job 2 has no ceiling → group has no ceiling
	if groups[0].MaxGPUMemGB != 0 {
		t.Errorf("MaxGPUMemGB = %d, want 0 (no ceiling)", groups[0].MaxGPUMemGB)
	}
}

func TestMergeCompatibleGroups_MergesCompatible(t *testing.T) {
	// Two groups with same GPU class and no image → merged into one
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged group, got %d", len(merged))
	}
	if len(merged[0].Jobs) != 2 {
		t.Errorf("expected 2 jobs in merged group, got %d", len(merged[0].Jobs))
	}
}

func TestMergeCompatibleGroups_PreservesIncompatible(t *testing.T) {
	// H100 and A100 are incompatible pinned models → stay separate
	groups := []InstanceGroup{
		{GPUClass: "H100", GPUMemGB: 80, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "A100", GPUMemGB: 40, Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 2 {
		t.Fatalf("expected 2 groups (incompatible GPUs), got %d", len(merged))
	}
}

func TestMergeCompatibleGroups_TakesMemorySupremum(t *testing.T) {
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, MaxGPUMemGB: 48, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 40, MaxGPUMemGB: 0, Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged group, got %d", len(merged))
	}
	if merged[0].GPUMemGB != 40 {
		t.Errorf("expected GPUMemGB=40 (supremum), got %d", merged[0].GPUMemGB)
	}
	if merged[0].MaxGPUMemGB != 0 {
		t.Errorf("expected MaxGPUMemGB=0 (one uncapped), got %d", merged[0].MaxGPUMemGB)
	}
}

func TestMergeCompatibleGroups_SingleGroup(t *testing.T) {
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 1 {
		t.Fatalf("expected 1 group, got %d", len(merged))
	}
}

func TestMergeCompatibleGroups_DoesNotMutateOriginal(t *testing.T) {
	job1 := &db.Job{ID: 1}
	job2 := &db.Job{ID: 2}
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{job1}},
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{job2}},
	}
	_ = MergeCompatibleGroups(groups)
	// Original groups should still have 1 job each
	if len(groups[0].Jobs) != 1 || len(groups[1].Jobs) != 1 {
		t.Error("MergeCompatibleGroups mutated the original groups")
	}
}

func TestMergeGPUMemCeiling(t *testing.T) {
	tests := []struct {
		a, b, want int
	}{
		{0, 0, 0},    // both uncapped → uncapped
		{24, 0, 0},   // one uncapped → uncapped
		{0, 48, 0},   // one uncapped → uncapped
		{24, 48, 48}, // both capped → max
		{48, 24, 48}, // both capped → max
		{24, 24, 24}, // same → same
	}
	for _, tt := range tests {
		got := mergeGPUMemCeiling(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("mergeGPUMemCeiling(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
