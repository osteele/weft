package campaign

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/cloud"
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

func TestGroupByAffinity_DifferentProviderTagsDoNotMerge(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:       1,
			Status:   db.StatusQueued,
			GPUClass: "nvidia",
			GPUMemGB: intPtr(24),
			Inputs:   []string{"hf:meta-llama/Llama-3-8B"},
			Tags:     []string{"provider:vastai"},
		},
		{
			ID:       2,
			Status:   db.StatusQueued,
			GPUClass: "nvidia",
			GPUMemGB: intPtr(24),
			Inputs:   []string{"hf:meta-llama/Llama-3-8B"},
			Tags:     []string{"provider:runpod"},
		},
	}

	groups := GroupByAffinity(jobs, nil)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].Provider == groups[1].Provider {
		t.Fatalf("expected distinct providers, got %q and %q", groups[0].Provider, groups[1].Provider)
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

func TestGroupCeilingDefaultsToVRAMTier(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			GPUMemMaxGB: intPtr(24),
			Inputs:      []string{"hf:model-a"}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:model-a"}}, // no explicit ceiling
	}
	groups := GroupByAffinity(jobs, nil)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	// Job 2 has no explicit ceiling → defaults to VRAM tier (24)
	// mergeGPUMemCeiling(24, 24) = 24
	if groups[0].MaxGPUMemGB != 24 {
		t.Errorf("MaxGPUMemGB = %d, want 24 (VRAM tier default)", groups[0].MaxGPUMemGB)
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
	// Groups in the same VRAM tier merge and take the supremum
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, MaxGPUMemGB: 24, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 22, MaxGPUMemGB: 24, Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged group (same tier), got %d", len(merged))
	}
	if merged[0].GPUMemGB != 22 {
		t.Errorf("expected GPUMemGB=22 (supremum), got %d", merged[0].GPUMemGB)
	}
}

func TestMergeCompatibleGroups_DifferentTiersStaySeparate(t *testing.T) {
	// Groups in different VRAM tiers (24 vs 48) should NOT merge
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 40, Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 2 {
		t.Fatalf("expected 2 groups (different VRAM tiers), got %d", len(merged))
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

func TestGroupByAffinity_SeparatesMemoryTiers(t *testing.T) {
	// Jobs with different VRAM tiers should form separate groups even with
	// shared inputs. This prevents 8GB jobs from being routed to expensive
	// GPUs just because they share a group with a 20GB job.
	jobs := []*db.Job{
		{ID: 545, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 546, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 547, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 548, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 549, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
	}
	groups := GroupByAffinity(jobs, nil)
	if len(groups) < 2 {
		t.Fatalf("expected at least 2 groups (different VRAM tiers), got %d", len(groups))
	}
	// The 20GB job (tier 24) should be in its own group
	var found20GB bool
	for _, g := range groups {
		if g.GPUMemGB == 20 && len(g.Jobs) == 1 {
			found20GB = true
		}
	}
	if !found20GB {
		t.Error("expected the 20GB job to be in its own group")
	}
	// The 8GB jobs (tier 12) should be grouped together with VRAM tier ceiling
	var group8GB *InstanceGroup
	for i := range groups {
		if groups[i].GPUMemGB == 8 && len(groups[i].Jobs) == 4 {
			group8GB = &groups[i]
		}
	}
	if group8GB == nil {
		t.Fatal("expected the four 8GB jobs to be grouped together")
	}
	if group8GB.MaxGPUMemGB != 12 {
		t.Errorf("8GB group MaxGPUMemGB = %d, want 12 (VRAM tier)", group8GB.MaxGPUMemGB)
	}
}

// TestGroupByAffinity_Job545Through549 reproduces the actual job scenario:
// job 545 (gpu_mem=20) and jobs 546-549 (gpu_mem=8) all have gpu_class=nvidia
// and share inputs hf:gpt2 + hf-dataset:wikitext. They should form 2 groups
// (tier 24 and tier 12), not 1 merged group.
func TestGroupByAffinity_Job545Through549(t *testing.T) {
	jobs := []*db.Job{
		{ID: 542, Status: db.StatusQueued, GPUClass: "3090", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:gpt2"}},
		{ID: 545, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 546, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 547, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 548, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 549, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 550, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:EleutherAI/pythia-1.4b"}},
	}
	groups := GroupByAffinity(jobs, nil)

	// Expect at least 4 groups:
	// 1. 3090 (job 542) — pinned GPU class
	// 2. NVIDIA ≥20GB (job 545) — tier 24, shares gpt2 inputs but different tier from 546-549
	// 3. NVIDIA ≥8GB (jobs 546-549) — tier 12, share gpt2 inputs
	// 4. NVIDIA ≥20GB (job 550) — tier 24, different inputs from job 545
	if len(groups) < 4 {
		t.Fatalf("expected at least 4 groups, got %d", len(groups))
		for i, g := range groups {
			t.Logf("  group %d: %s ≥%dGB, %d jobs", i, g.GPUClass, g.GPUMemGB, len(g.Jobs))
		}
	}

	// Verify the 8GB jobs are in their own group
	var group8GB *InstanceGroup
	for i := range groups {
		if groups[i].GPUMemGB == 8 {
			group8GB = &groups[i]
			break
		}
	}
	if group8GB == nil {
		t.Fatal("no group with GPUMemGB=8 found")
	}
	if len(group8GB.Jobs) != 4 {
		t.Errorf("8GB group should have 4 jobs (546-549), got %d", len(group8GB.Jobs))
	}

	// Verify no group has a mix of 8GB and 20GB jobs
	for i, g := range groups {
		var has8, has20 bool
		for _, j := range g.Jobs {
			if j.GPUMemGB != nil {
				if *j.GPUMemGB <= 12 {
					has8 = true
				}
				if *j.GPUMemGB >= 20 {
					has20 = true
				}
			}
		}
		if has8 && has20 {
			t.Errorf("group %d mixes 8GB and 20GB jobs — should be separate tiers", i)
		}
	}

	t.Logf("Groups formed: %d", len(groups))
	for i, g := range groups {
		t.Logf("  group %d: %s ≥%dGB, %d jobs (IDs: %v)", i, g.GPUClass, g.GPUMemGB, len(g.Jobs), jobIDs(g.Jobs))
	}
}

func jobIDs(jobs []*db.Job) []int64 {
	ids := make([]int64, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids
}

func TestVramTierOf(t *testing.T) {
	tests := []struct {
		memGB int
		want  int
	}{
		{0, 12},
		{8, 12},
		{12, 12},
		{16, 16},
		{20, 24},
		{24, 24},
		{40, 48},
		{80, 80},
		{141, 141},
		{200, 200}, // above all tiers
	}
	for _, tt := range tests {
		got := vramTierOf(tt.memGB)
		if got != tt.want {
			t.Errorf("vramTierOf(%d) = %d, want %d", tt.memGB, got, tt.want)
		}
	}
}

func TestResolveJobVastCapAdd(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte(`# /// script
# [tool.weft]
# vast-cap-add = ["sys_admin", "NET_ADMIN"]
# ///
`), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	got := ResolveJobVastCapAdd(dir, "uv run python train.py")
	want := []string{"SYS_ADMIN", "NET_ADMIN"}
	if len(got) != len(want) {
		t.Fatalf("ResolveJobVastCapAdd() len=%d want=%d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ResolveJobVastCapAdd()[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestSplitGroupsByImage_UnionVastCapAdd(t *testing.T) {
	dir := t.TempDir()
	withCap := filepath.Join(dir, "with_cap.py")
	withoutCap := filepath.Join(dir, "without_cap.py")
	if err := os.WriteFile(withCap, []byte(`# /// script
# [tool.weft]
# vast-cap-add = ["SYS_ADMIN"]
# ///
`), 0o644); err != nil {
		t.Fatalf("write with_cap.py: %v", err)
	}
	if err := os.WriteFile(withoutCap, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write without_cap.py: %v", err)
	}

	groups := SplitGroupsByImage([]InstanceGroup{
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 24,
			Jobs: []*db.Job{
				{ID: 1, Command: "python with_cap.py", WorkingDir: dir},
				{ID: 2, Command: "python without_cap.py", WorkingDir: dir},
			},
		},
	})

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].VastCapAdd) != 1 || groups[0].VastCapAdd[0] != "SYS_ADMIN" {
		t.Fatalf("group.VastCapAdd=%v want [SYS_ADMIN]", groups[0].VastCapAdd)
	}
	if len(groups[0].Jobs) != 2 {
		t.Fatalf("expected both jobs in same group, got %d jobs", len(groups[0].Jobs))
	}
}

func TestSplitGroupsByImage_AutoUpgradesDefaultImageForBlackwell(t *testing.T) {
	groups := SplitGroupsByImage([]InstanceGroup{
		{
			GPUClass: "RTX-5090",
			GPUMemGB: 20,
			Jobs: []*db.Job{
				{ID: 1, Command: "python train.py"},
			},
		},
	})

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Image != "nvidia/cuda:12.8.1-runtime-ubuntu22.04" {
		t.Fatalf("Image = %q, want nvidia/cuda:12.8.1-runtime-ubuntu22.04", groups[0].Image)
	}
}

func TestSplitGroupsByImage_UpgradesExplicitOlderCUDAForBlackwell(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".weft.toml")
	if err := os.WriteFile(cfg, []byte("[cloud]\nimage = \"nvidia/cuda:12.4.1-runtime-ubuntu22.04\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	groups := SplitGroupsByImage([]InstanceGroup{
		{
			GPUClass: "RTX-5090",
			GPUMemGB: 20,
			Jobs: []*db.Job{
				{ID: 1, Command: "python train.py", WorkingDir: dir},
			},
		},
	})

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Image != "nvidia/cuda:12.8.1-runtime-ubuntu22.04" {
		t.Fatalf("Image = %q, want nvidia/cuda:12.8.1-runtime-ubuntu22.04", groups[0].Image)
	}
}

func TestSplitGroupsByImage_LeavesExplicitNonCUDAImageUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".weft.toml")
	if err := os.WriteFile(cfg, []byte("[cloud]\nimage = \"ubuntu:22.04\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	groups := SplitGroupsByImage([]InstanceGroup{
		{
			GPUClass: "RTX-5090",
			GPUMemGB: 20,
			Jobs: []*db.Job{
				{ID: 1, Command: "python train.py", WorkingDir: dir},
			},
		},
	})

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Image != "ubuntu:22.04" {
		t.Fatalf("Image = %q, want ubuntu:22.04", groups[0].Image)
	}
}

func TestSplitGroupsByImage_RunpodRemapsExplicitPyTorchImage(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".weft.toml")
	if err := os.WriteFile(cfg, []byte("[cloud]\nimage = \"pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	groups := SplitGroupsByImage([]InstanceGroup{
		{
			Provider: "runpod",
			GPUClass: "RTX_4090",
			GPUMemGB: 24,
			Jobs: []*db.Job{
				{ID: 1, Command: "python train.py", WorkingDir: dir},
			},
		},
	})

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Image != cloud.DefaultRunpodImage {
		t.Fatalf("Image = %q, want %q", groups[0].Image, cloud.DefaultRunpodImage)
	}
}

func TestSplitGroupsByImage_RunpodKeepsRunpodImage(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".weft.toml")
	if err := os.WriteFile(cfg, []byte("[cloud]\nimage = \"runpod/pytorch:2.6.0-py3.11-cuda12.4.1-cudnn-devel-ubuntu22.04\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	groups := SplitGroupsByImage([]InstanceGroup{
		{
			Provider: "runpod",
			GPUClass: "RTX_4090",
			GPUMemGB: 24,
			Jobs: []*db.Job{
				{ID: 1, Command: "python train.py", WorkingDir: dir},
			},
		},
	})

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Image != "runpod/pytorch:2.6.0-py3.11-cuda12.4.1-cudnn-devel-ubuntu22.04" {
		t.Fatalf("Image = %q", groups[0].Image)
	}
}

func TestSplitToParallel_MultiJobGroup(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		GPUMemGB: 20, // supremum of 8, 8, 20
		DiskGB:   50,
		Image:    "cuda:12.4",
		Jobs: []*db.Job{
			{ID: 1, GPUMemGB: intPtr(20), GPUMemMaxGB: intPtr(24)},
			{ID: 2, GPUMemGB: intPtr(8)},
			{ID: 3, GPUMemGB: intPtr(8)},
		},
	}
	result := SplitToParallel([]InstanceGroup{group})
	if len(result) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(result))
	}
	// Each group should have exactly 1 job
	for i, g := range result {
		if len(g.Jobs) != 1 {
			t.Errorf("group %d: expected 1 job, got %d", i, len(g.Jobs))
		}
		if g.GPUClass != "NVIDIA" {
			t.Errorf("group %d: GPUClass = %q, want NVIDIA", i, g.GPUClass)
		}
		if g.DiskGB != 50 {
			t.Errorf("group %d: DiskGB = %d, want 50", i, g.DiskGB)
		}
		if g.Image != "cuda:12.4" {
			t.Errorf("group %d: Image = %q, want cuda:12.4", i, g.Image)
		}
	}
	// Jobs should use their individual GPU memory, not the group supremum.
	// sorted by GPUMemGB descending, so the 20GB job comes first.
	if result[0].GPUMemGB != 20 {
		t.Errorf("first group GPUMemGB = %d, want 20", result[0].GPUMemGB)
	}
	if result[0].MaxGPUMemGB != 24 {
		t.Errorf("first group MaxGPUMemGB = %d, want 24", result[0].MaxGPUMemGB)
	}
	// The 8GB jobs should have their own GPUMemGB and a VRAM tier ceiling
	for _, g := range result[1:] {
		if g.GPUMemGB != 8 {
			continue
		}
		if g.MaxGPUMemGB != 12 {
			t.Errorf("8GB group (job %d): MaxGPUMemGB = %d, want 12 (VRAM tier default)",
				g.Jobs[0].ID, g.MaxGPUMemGB)
		}
	}
}

func TestSplitToParallel_SingleJobGroup(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "RTX_3090",
		GPUMemGB: 24,
		Jobs:     []*db.Job{{ID: 1, GPUMemGB: intPtr(24)}},
	}
	result := SplitToParallel([]InstanceGroup{group})
	if len(result) != 1 {
		t.Fatalf("expected 1 group, got %d", len(result))
	}
	if len(result[0].Jobs) != 1 {
		t.Errorf("expected 1 job, got %d", len(result[0].Jobs))
	}
	if result[0].GPUMemGB != 24 {
		t.Errorf("GPUMemGB = %d, want 24", result[0].GPUMemGB)
	}
}

func TestSplitToParallel_EmptyInput(t *testing.T) {
	result := SplitToParallel(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 groups, got %d", len(result))
	}
}
