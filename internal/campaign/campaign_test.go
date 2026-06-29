package campaign

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
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

	// Unconstrained job (ID=4) forms its own group, and pinned GPU jobs in
	// different VRAM tiers stay separate so smaller jobs are not blocked by a
	// larger tier's capacity.
	if len(groups) != 4 {
		t.Fatalf("expected 4 groups, got %d", len(groups))
	}

	// Groups sorted by descending memory: H100 80GB group first.
	if groups[0].GPUMemGB != 80 {
		t.Errorf("first group should have 80GB, got %d", groups[0].GPUMemGB)
	}
	if groups[0].GPUClass != "H100" {
		t.Errorf("first group should be H100, got %s", groups[0].GPUClass)
	}
	if len(groups[0].Jobs) != 2 {
		t.Errorf("H100 80GB group should have 2 jobs, got %d", len(groups[0].Jobs))
	}

	// A100 40GB group.
	if groups[1].GPUClass != "A100" {
		t.Errorf("second group should be A100, got %s", groups[1].GPUClass)
	}
	if groups[1].GPUMemGB != 40 {
		t.Errorf("second group should have 40GB, got %d", groups[1].GPUMemGB)
	}

	// H100 40GB group.
	if groups[2].GPUClass != "H100" {
		t.Errorf("third group should be H100, got %s", groups[2].GPUClass)
	}
	if groups[2].GPUMemGB != 40 {
		t.Errorf("third group should have 40GB, got %d", groups[2].GPUMemGB)
	}

	// Unconstrained group.
	if groups[3].GPUClass != "" {
		t.Errorf("fourth group should be unconstrained, got %s", groups[3].GPUClass)
	}
	if groups[3].GPUMemGB != 24 {
		t.Errorf("fourth group should have 24GB, got %d", groups[3].GPUMemGB)
	}
	if len(groups[3].Jobs) != 1 {
		t.Errorf("unconstrained group should have 1 job, got %d", len(groups[3].Jobs))
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
		{InstanceGroup{GPUClass: "A100", GPUMemGB: 24, MaxGPUMemGB: 48}, "A100 ≥24GB"},
		{InstanceGroup{GPUMemGB: 24, MaxGPUMemGB: 24}, "≥24GB"},
		{InstanceGroup{GPUClass: "V100", GPUMemGB: 22, MaxGPUMemGB: 20}, "V100 ≥22GB"},
		{InstanceGroup{GPUClass: "AMPERE+", NumGPUs: 2, GPUMemGB: 40}, "2x AMPERE+ ≥40GB"},
	}
	for _, tt := range tests {
		got := tt.group.GPUSpec()
		if got != tt.want {
			t.Errorf("GPUSpec() = %q, want %q", got, tt.want)
		}
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

func TestMergeCompatibleGroups_DifferentRunpodCloudTypesStaySeparate(t *testing.T) {
	groups := []InstanceGroup{
		{GPUClass: "L4", Provider: "runpod", RunpodCloudType: cloud.RunpodCloudTypeSecure, GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "L4", Provider: "runpod", GPUMemGB: 20, Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 2 {
		t.Fatalf("expected 2 groups (different RunPod cloud types), got %d", len(merged))
	}
}

func TestMergeCompatibleGroups_TakesMemorySupremum(t *testing.T) {
	// Groups in the same VRAM tier merge and take the supremum
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 22, Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged group (same tier), got %d", len(merged))
	}
	if merged[0].GPUMemGB != 22 {
		t.Errorf("expected GPUMemGB=22 (supremum), got %d", merged[0].GPUMemGB)
	}
}

func TestMergeCompatibleGroups_PreservesMostRestrictiveMaxComputeCap(t *testing.T) {
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 82, MaxComputeCap: "12.0", Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 82, MaxComputeCap: "9.0", Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged group, got %d", len(merged))
	}
	if merged[0].MaxComputeCap != "9.0" {
		t.Fatalf("MaxComputeCap = %q, want 9.0", merged[0].MaxComputeCap)
	}
}

func TestMergeCompatibleGroups_PreservesMostRestrictiveMinComputeCap(t *testing.T) {
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 24, MinComputeCap: "7.5", Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 24, MinComputeCap: "8.0", Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 1 {
		t.Fatalf("len(merged) = %d, want 1", len(merged))
	}
	if merged[0].MinComputeCap != "8.0" {
		t.Fatalf("MinComputeCap = %q, want 8.0", merged[0].MinComputeCap)
	}
}

func TestMergeCompatibleGroups_PreservesResourceShape(t *testing.T) {
	groups := []InstanceGroup{
		{
			GPUClass:     "AMPERE+",
			NumGPUs:      2,
			GPUMemGB:     40,
			CPUCores:     12,
			Interconnect: "any",
			Jobs:         []*db.Job{{ID: 1}},
		},
		{
			GPUClass:     "AMPERE+",
			NumGPUs:      2,
			GPUMemGB:     42,
			CPUCores:     24,
			Interconnect: "any",
			Jobs:         []*db.Job{{ID: 2}},
		},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 1 {
		t.Fatalf("len(merged) = %d, want 1", len(merged))
	}
	if merged[0].NumGPUs != 2 {
		t.Fatalf("NumGPUs = %d, want 2", merged[0].NumGPUs)
	}
	if merged[0].CPUCores != 24 {
		t.Fatalf("CPUCores = %d, want 24", merged[0].CPUCores)
	}
	if merged[0].Interconnect != "any" {
		t.Fatalf("Interconnect = %q, want any", merged[0].Interconnect)
	}
}

func TestMergeCompatibleGroups_DifferentGPUCountsStaySeparate(t *testing.T) {
	groups := []InstanceGroup{
		{GPUClass: "AMPERE+", NumGPUs: 1, GPUMemGB: 40, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "AMPERE+", NumGPUs: 2, GPUMemGB: 40, Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
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

// Regression: MergeCompatibleGroups previously dropped MinCUDAVersion and
// MinDriverVersion when constructing merged/new groups, defeating the CUDA
// back-fill set up by SplitGroupsByImage / ResolveJobImageSettings. The merged
// candidate would then search Vast with no driver_version predicate.
func TestMergeCompatibleGroups_PreservesCUDADriverFloors(t *testing.T) {
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 24, MinCUDAVersion: "12.4", MinDriverVersion: 550, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 24, MinCUDAVersion: "12.8", MinDriverVersion: 570, Jobs: []*db.Job{{ID: 2}}},
	}
	merged := MergeCompatibleGroups(groups)
	if len(merged) != 1 {
		t.Fatalf("len(merged) = %d, want 1", len(merged))
	}
	if merged[0].MinCUDAVersion != "12.8" {
		t.Errorf("MinCUDAVersion = %q, want 12.8 (max of merged groups)", merged[0].MinCUDAVersion)
	}
	if merged[0].MinDriverVersion != 570 {
		t.Errorf("MinDriverVersion = %d, want 570 (max of merged groups)", merged[0].MinDriverVersion)
	}
}

// Regression: SplitToParallel previously dropped MinCUDAVersion and
// MinDriverVersion when expanding a multi-job group into per-job groups, so
// the parallel candidate would search with no driver filter.
func TestSplitToParallel_PreservesCUDADriverFloors(t *testing.T) {
	groups := []InstanceGroup{
		{
			GPUClass:         "NVIDIA",
			GPUMemGB:         24,
			MinCUDAVersion:   "12.8",
			MinDriverVersion: 570,
			Jobs: []*db.Job{
				{ID: 1},
				{ID: 2},
			},
		},
	}
	result := SplitToParallel(groups)
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}
	for i, g := range result {
		if g.MinCUDAVersion != "12.8" {
			t.Errorf("result[%d].MinCUDAVersion = %q, want 12.8", i, g.MinCUDAVersion)
		}
		if g.MinDriverVersion != 570 {
			t.Errorf("result[%d].MinDriverVersion = %d, want 570", i, g.MinDriverVersion)
		}
	}
}

func TestSplitToParallel_PreservesResourceShape(t *testing.T) {
	groups := []InstanceGroup{
		{
			GPUClass:     "AMPERE+",
			NumGPUs:      2,
			GPUMemGB:     42,
			CPUCores:     24,
			Interconnect: "any",
			Jobs: []*db.Job{
				{ID: 1, GPUMemGB: intPtr(40)},
				{ID: 2, GPUMemGB: intPtr(42)},
			},
		},
	}
	result := SplitToParallel(groups)
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}
	for i, g := range result {
		if g.NumGPUs != 2 {
			t.Errorf("result[%d].NumGPUs = %d, want 2", i, g.NumGPUs)
		}
		if g.CPUCores != 24 {
			t.Errorf("result[%d].CPUCores = %d, want 24", i, g.CPUCores)
		}
		if g.Interconnect != "any" {
			t.Errorf("result[%d].Interconnect = %q, want any", i, g.Interconnect)
		}
	}
}

func TestAssetOverlapBytes_IncludesDeclaredAndObservedHFAssets(t *testing.T) {
	groups := []InstanceGroup{
		{
			Jobs: []*db.Job{
				{ID: 1, Inputs: []string{"hf:model-a", "hf-dataset:data-a", "checkpoint:ckpt-a"}},
				{ID: 2, ObservedInputs: []string{"hf:model-a", "hf-dataset:data-a", "checkpoint:ckpt-a"}},
			},
		},
	}

	got := assetOverlapBytes(groups)
	want := 2 * refCountWeight
	if got != want {
		t.Fatalf("assetOverlapBytes = %v, want %v", got, want)
	}
}

func TestAssetOverlapDoesNotRelaxMergeCompatibility(t *testing.T) {
	groups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 24, Image: "ghcr.io/example/a:latest", Jobs: []*db.Job{{ID: 1, Inputs: []string{"hf:model-a"}}}},
		{GPUClass: "NVIDIA", GPUMemGB: 24, Image: "ghcr.io/example/b:latest", Jobs: []*db.Job{{ID: 2, Inputs: []string{"hf:model-a"}}}},
	}

	merged := MergeCompatibleGroups(groups)
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
	}
}

func TestScoreGroupingWithOverlapCanPreferOverlapCandidate(t *testing.T) {
	profile := bidding.ScoreProfile{
		ID:               "test",
		Weights_:         bidding.StrategyWeights{Cost: 1, Time: 10},
		UseHappyPathTime: true,
	}
	offer := &cloud.Offer{ProviderID: "offer"}
	noOverlap := []CostEstimate{{
		Group:     InstanceGroup{Jobs: []*db.Job{{ID: 1}}},
		Offer:     GroupOffer{Offer: offer},
		TotalTime: time.Hour,
		TotalCost: 1.00,
	}}
	withOverlap := []CostEstimate{{
		Group:     InstanceGroup{Jobs: []*db.Job{{ID: 1}}},
		Offer:     GroupOffer{Offer: offer},
		TotalTime: time.Hour,
		TotalCost: 1.05,
	}}

	noOverlapScore := scoreGroupingWithOverlap(noOverlap, profile, 0)
	overlapScore := scoreGroupingWithOverlap(withOverlap, profile, 0.01)
	if overlapScore >= noOverlapScore {
		t.Fatalf("overlap score = %v, want less than no-overlap score %v", overlapScore, noOverlapScore)
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
	// The 8GB jobs should be grouped together.
	var group8GB *InstanceGroup
	for i := range groups {
		if groups[i].GPUMemGB == 8 && len(groups[i].Jobs) == 4 {
			group8GB = &groups[i]
		}
	}
	if group8GB == nil {
		t.Fatal("expected the four 8GB jobs to be grouped together")
	}
}

func TestGroupByAffinity_SeparatesRunpodCloudTypes(t *testing.T) {
	jobs := []*db.Job{
		{
			ID: 1, Status: db.StatusQueued, GPUClass: "l4", GPUMemGB: intPtr(20), Tags: []string{"provider:runpod"},
			CLIResourceOverrides: &db.CLIResourceOverrides{RunpodCloudType: cloud.RunpodCloudTypeSecure},
		},
		{ID: 2, Status: db.StatusQueued, GPUClass: "l4", GPUMemGB: intPtr(20), Tags: []string{"provider:runpod"}},
	}
	groups := GroupByAffinity(jobs, nil)
	if len(groups) != 2 {
		t.Fatalf("len(groups) = %d, want 2", len(groups))
	}
	var secure, defaulted bool
	for _, group := range groups {
		switch group.RunpodCloudType {
		case cloud.RunpodCloudTypeSecure:
			secure = true
		case "":
			defaulted = true
		default:
			t.Fatalf("unexpected RunpodCloudType %q", group.RunpodCloudType)
		}
	}
	if !secure || !defaulted {
		t.Fatalf("secure=%v defaulted=%v, want both true", secure, defaulted)
	}
}

func TestGroupByAffinity_PinnedGPUSeparatesMemoryTiers(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1714, Status: db.StatusQueued, GPUClass: "a100", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3.1-8B"}},
		{ID: 1715, Status: db.StatusQueued, GPUClass: "a100", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3.1-8B"}},
		{ID: 1716, Status: db.StatusQueued, GPUClass: "a100", GPUMemGB: intPtr(82),
			Inputs: []string{"hf:meta-llama/Llama-3.1-8B"}},
		{ID: 1718, Status: db.StatusQueued, GPUClass: "a100", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:meta-llama/Llama-3.1-8B"}},
	}

	groups := GroupByAffinity(jobs, nil)

	var group20, group82 *InstanceGroup
	for i := range groups {
		switch groups[i].GPUMemGB {
		case 20:
			group20 = &groups[i]
		case 82:
			group82 = &groups[i]
		}
	}
	if group20 == nil {
		t.Fatalf("missing A100 20GB group; got groups: %v", groupSummaries(groups))
	}
	if group82 == nil {
		t.Fatalf("missing A100 82GB group; got groups: %v", groupSummaries(groups))
	}
	if got := jobIDs(group20.Jobs); !sameInt64s(got, []int64{1714, 1715, 1718}) {
		t.Fatalf("A100 20GB group jobs = %v, want [1714 1715 1718]", got)
	}
	if got := jobIDs(group82.Jobs); !sameInt64s(got, []int64{1716}) {
		t.Fatalf("A100 82GB group jobs = %v, want [1716]", got)
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

func sameInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func groupSummaries(groups []InstanceGroup) []string {
	summaries := make([]string, 0, len(groups))
	for _, group := range groups {
		summaries = append(summaries, fmt.Sprintf("%s >=%dGB jobs=%v", group.GPUClass, group.GPUMemGB, jobIDs(group.Jobs)))
	}
	return summaries
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

func TestResolveJobImageSettings_ImageOverridePattern(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".weft.toml"), []byte(`[cloud]
image = "ghcr.io/example/project:latest"

[cloud.image-overrides]
"train*.py" = "ghcr.io/example/train:cuda129"
`), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "train_model.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(dir, "uv run python train_model.py")
	if img != "ghcr.io/example/train:cuda129" {
		t.Fatalf("image = %q, want pattern override", img)
	}
}

func TestResolveJobImageSettings_ScriptImageBeatsPattern(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".weft.toml"), []byte(`[cloud.image-overrides]
"train.py" = "ghcr.io/example/train:cuda129"
`), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte(`# /// script
# [tool.weft]
# image = "ghcr.io/example/script:cuda130"
# ///
`), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(dir, "python train.py")
	if img != "ghcr.io/example/script:cuda130" {
		t.Fatalf("image = %q, want script image", img)
	}
}

func TestResolveJobImageSettings_UnmatchedPatternFallsBackToProjectImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".weft.toml"), []byte(`[cloud]
image = "ghcr.io/example/project:latest"

[cloud.image-overrides]
"train*.py" = "ghcr.io/example/train:cuda129"
`), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "eval.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(dir, "python eval.py")
	if img != "ghcr.io/example/project:latest" {
		t.Fatalf("image = %q, want project image", img)
	}
}

func TestResolveJobImageSettings_NestedPatternMatchesRelativeToProject(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".weft.toml"), []byte(`[cloud.image-overrides]
"scripts/infer.py" = "ghcr.io/example/infer:cuda124"
`), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "infer.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(subdir, "python infer.py")
	if img != "ghcr.io/example/infer:cuda124" {
		t.Fatalf("image = %q, want nested pattern image", img)
	}
}

func TestResolveJobImageSettings_OverlappingPatternsChooseLongest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".weft.toml"), []byte(`[cloud.image-overrides]
"train*.py" = "ghcr.io/example/generic:latest"
"train_big.py" = "ghcr.io/example/big:latest"
`), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "train_big.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(dir, "python train_big.py")
	if img != "ghcr.io/example/big:latest" {
		t.Fatalf("image = %q, want longest pattern image", img)
	}
}

func TestResolveJobImageSettings_OverlappingPatternsTieBreakLexically(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".weft.toml"), []byte(`[cloud.image-overrides]
"train_*.py" = "ghcr.io/example/first:latest"
"train_?.py" = "ghcr.io/example/second:latest"
`), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "train_a.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(dir, "python train_a.py")
	if img != "ghcr.io/example/first:latest" {
		t.Fatalf("image = %q, want lexical tie-break image", img)
	}
}

func TestResolveJobImageSettings_SGLangCommandUsesRuntimeImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "profile_inference_sglang.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(dir, "python profile_inference_sglang.py")
	if img != sglangRuntimeImage {
		t.Fatalf("image = %q, want %q", img, sglangRuntimeImage)
	}
}

func TestResolveJobImageSettings_ProjectImageBeatsSGLangInference(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".weft.toml"), []byte("[cloud]\nimage = \"ghcr.io/example/custom-sglang:latest\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile_inference_sglang.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(dir, "python profile_inference_sglang.py")
	if img != "ghcr.io/example/custom-sglang:latest" {
		t.Fatalf("image = %q, want explicit project image", img)
	}
}

func TestResolveJobImageSettings_SGLangLegacyImageAliasesToPublic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infer.py"), []byte(`# /// script
# [tool.weft]
# image = "ghcr.io/osteele/sglang-runtime:v0.5.10.post1"
# ///
print('ok')
`), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(dir, "python infer.py")
	if img != sglangRuntimeImage {
		t.Fatalf("image = %q, want public SGLang runtime %q", img, sglangRuntimeImage)
	}
}

func TestResolveJobImageSettings_SGLangPyprojectUsesRuntimeImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"sglang[srt]>=0.4\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}

	img, _, _ := ResolveJobImageSettings(dir, "python infer.py")
	if img != sglangRuntimeImage {
		t.Fatalf("image = %q, want %q", img, sglangRuntimeImage)
	}
}

func TestValidateJobImageAvailability_ProbesResolvedImageWithAuth(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".weft.toml"), []byte("[cloud]\nimage = \"ghcr.io/acme/private:tag\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}
	t.Setenv("WEFT_TEST_REGISTRY_PASSWORD", "secret-token")
	cfg := &config.Config{
		Registry: map[string]config.RegistryConfig{
			"ghcr.io": {
				Username:    "osteele",
				PasswordEnv: "WEFT_TEST_REGISTRY_PASSWORD",
			},
		},
	}

	prev := imageRequirementResolver
	t.Cleanup(func() { imageRequirementResolver = prev })
	called := false
	imageRequirementResolver = func(ctx context.Context, image string, auth *cloud.RegistryAuth) (cloud.ImageRequirements, error) {
		called = true
		if image != "ghcr.io/acme/private:tag" {
			t.Fatalf("image = %q, want configured private image", image)
		}
		if auth == nil || auth.Host != "ghcr.io" || auth.Username != "osteele" || auth.Password != "secret-token" {
			t.Fatalf("auth = %+v, want ghcr credentials", auth)
		}
		return cloud.ImageRequirements{}, nil
	}

	if err := ValidateJobImageAvailability(context.Background(), cfg, dir, "python train.py"); err != nil {
		t.Fatalf("ValidateJobImageAvailability: %v", err)
	}
	if !called {
		t.Fatal("imageRequirementResolver was not called")
	}
}

func TestValidateJobImageAvailability_ReportsProbeFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".weft.toml"), []byte("[cloud]\nimage = \"ghcr.io/acme/private:tag\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	prev := imageRequirementResolver
	t.Cleanup(func() { imageRequirementResolver = prev })
	denied := errors.New("unauthorized")
	imageRequirementResolver = func(ctx context.Context, image string, auth *cloud.RegistryAuth) (cloud.ImageRequirements, error) {
		return cloud.ImageRequirements{}, denied
	}

	err := ValidateJobImageAvailability(context.Background(), nil, dir, "python train.py")
	if !errors.Is(err, denied) {
		t.Fatalf("error = %v, want wrapped probe error", err)
	}
	if !strings.Contains(err.Error(), "is not pullable with configured auth") {
		t.Fatalf("error = %q, want pullability context", err)
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

	groups := SplitGroupsByImage(nil, []InstanceGroup{
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

func TestSplitGroupsByImage_PreservesResourceShape(t *testing.T) {
	groups := SplitGroupsByImage(nil, []InstanceGroup{
		{
			GPUClass:     "AMPERE+",
			NumGPUs:      2,
			GPUMemGB:     40,
			CPUCores:     24,
			Interconnect: "any",
			Jobs:         []*db.Job{{ID: 1, Command: "python train.py"}},
		},
	})
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}
	if groups[0].NumGPUs != 2 {
		t.Fatalf("NumGPUs = %d, want 2", groups[0].NumGPUs)
	}
	if groups[0].CPUCores != 24 {
		t.Fatalf("CPUCores = %d, want 24", groups[0].CPUCores)
	}
	if groups[0].Interconnect != "any" {
		t.Fatalf("Interconnect = %q, want any", groups[0].Interconnect)
	}
}

func TestSplitGroupsByImage_AutoUpgradesDefaultImageForBlackwell(t *testing.T) {
	groups := SplitGroupsByImage(nil, []InstanceGroup{
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

	groups := SplitGroupsByImage(nil, []InstanceGroup{
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

func TestSplitGroupsByImage_AppliesCLIMinCUDAOverride(t *testing.T) {
	groups := SplitGroupsByImage(nil, []InstanceGroup{
		{
			GPUClass: "A100",
			GPUMemGB: 40,
			Jobs: []*db.Job{
				{
					ID:      1,
					Command: "python train.py",
					CLIResourceOverrides: &db.CLIResourceOverrides{
						MinCUDAVersion: "12.8",
					},
				},
			},
		},
	})

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].MinCUDAVersion != "12.8" {
		t.Fatalf("MinCUDAVersion = %q, want 12.8 (from CLI override)", groups[0].MinCUDAVersion)
	}
}

func TestSplitGroupsByImage_CLIMinCUDAOverridesProjectFloor(t *testing.T) {
	// CLI --cuda-driver-min is the highest-precedence explicit level: it
	// REPLACES the project floor and may lower it (the user's escape hatch
	// when the inferred/configured floor is wrong for their stack).
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".weft.toml")
	// MinCUDA in [cloud] is the project-level explicit floor.
	if err := os.WriteFile(cfg, []byte("[cloud]\nmin_cuda = \"12.6\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}
	groups := SplitGroupsByImage(nil, []InstanceGroup{
		{
			GPUClass: "A100",
			GPUMemGB: 40,
			Jobs: []*db.Job{
				{
					ID:         1,
					Command:    "python train.py",
					WorkingDir: dir,
					CLIResourceOverrides: &db.CLIResourceOverrides{
						MinCUDAVersion: "12.4",
					},
				},
			},
		},
	})

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].MinCUDAVersion != "12.4" {
		t.Fatalf("MinCUDAVersion = %q, want 12.4 (CLI override replaces project floor)", groups[0].MinCUDAVersion)
	}
	if groups[0].MinDriverVersion != 550 {
		t.Fatalf("MinDriverVersion = %d, want 550 (backfilled from CLI 12.4)", groups[0].MinDriverVersion)
	}
}

func TestSplitGroupsByImage_LeavesExplicitNonCUDAImageUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".weft.toml")
	if err := os.WriteFile(cfg, []byte("[cloud]\nimage = \"ubuntu:22.04\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	groups := SplitGroupsByImage(nil, []InstanceGroup{
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

	groups := SplitGroupsByImage(nil, []InstanceGroup{
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

	groups := SplitGroupsByImage(nil, []InstanceGroup{
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

func TestSplitGroupsByImage_ReadsCloudRequirementsFromTildeWorkingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projectDir := filepath.Join(home, "code", "research", "llm-performance-models")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".weft.toml"), []byte("[cloud]\nmin_driver = \"570\"\nmin_cuda = \"12.8\"\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	groups := SplitGroupsByImage(nil, []InstanceGroup{{
		GPUClass: "V100",
		Jobs: []*db.Job{{
			ID:         1241,
			WorkingDir: "~/code/research/llm-performance-models",
			Command:    "uv run python scripts/profile_inference_vllm.py",
		}},
	}})
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}
	if groups[0].MinDriverVersion != 570 || groups[0].MinCUDAVersion != "12.8" {
		t.Fatalf("requirements = driver %d cuda %q, want driver 570 cuda 12.8", groups[0].MinDriverVersion, groups[0].MinCUDAVersion)
	}
}

func TestSplitGroupsByImage_InferMinCUDAFromTorchLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(`
[[package]]
name = "torch"
version = "2.6.0"

[[package]]
name = "nvidia-cusparse-cu12"
version = "12.8.1.170"

[[package]]
name = "nvidia-nvjitlink-cu12"
version = "12.8.93"
`), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	groups := SplitGroupsByImage(nil, []InstanceGroup{{
		GPUClass: "NVIDIA",
		Jobs: []*db.Job{{
			ID:         1995,
			WorkingDir: dir,
			Command:    "uv run python scripts/profile_inference.py",
		}},
	}})
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}
	if groups[0].MinCUDAVersion != "12.8" {
		t.Fatalf("MinCUDAVersion = %q, want 12.8", groups[0].MinCUDAVersion)
	}
	// Auto-selected pytorch/* base images are skipped by image-label fetching
	// (shouldFetchImageRequirements), so weft must back-fill the driver floor
	// from the CUDA floor itself. Without this, Vast can return offers whose
	// advertised cuda_max_good clears `cuda>=12.8` while driver_version is
	// 525, and the actually-installed wheel fails at runtime with
	// cuda_driver_too_old.
	if groups[0].MinDriverVersion != 570 {
		t.Fatalf("MinDriverVersion = %d, want 570 (back-filled from CUDA 12.8)", groups[0].MinDriverVersion)
	}
}

func TestSplitGroupsByImage_InferMinCUDAFromUVRunWith(t *testing.T) {
	// Models the wj2305/wj2349 failure: project lockfile pins torch+cu124
	// (driver floor 550), but the submitted command uses
	// `uv run --with "vllm>=0.17"` which resolves a fresh vLLM at runtime.
	// The library-floor table escalates `vllm>=0.17` to the highest matching
	// row (vllm 0.20+ → CUDA 13.0), because the spec admits 0.20+; the
	// back-fill then sets driver to 580. A spec like `==0.17.0` would stay
	// at 12.8 / driver 570.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(`
[[package]]
name = "torch"
version = "2.6.0"

[[package]]
name = "nvidia-cublas-cu12"
version = "12.4.5.8"
`), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	groups := SplitGroupsByImage(nil, []InstanceGroup{{
		GPUClass: "NVIDIA",
		Jobs: []*db.Job{{
			ID:         2305,
			WorkingDir: dir,
			Command:    `uv run --with "vllm>=0.17" --with "pynvml>=12.0" python scripts/profile_inference_vllm.py`,
		}},
	}})
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}
	if groups[0].MinCUDAVersion != "13.0" {
		t.Fatalf("MinCUDAVersion = %q, want 13.0 (vllm>=0.17 admits 0.20+, which needs CUDA 13)", groups[0].MinCUDAVersion)
	}
	if groups[0].MinDriverVersion != 580 {
		t.Fatalf("MinDriverVersion = %d, want 580 (back-filled from CUDA 13.0)", groups[0].MinDriverVersion)
	}
}

func TestSplitGroupsByImage_TorchPinGovernsImageCUDA(t *testing.T) {
	// Regression: a project pinned to torch cu124 must get a cuda12.4 image,
	// even when the GPU family constraint (nvidia) admits Blackwell and would
	// otherwise upgrade the image to cuda12.8 — the pinned wheel cannot use a
	// newer CUDA runtime, and a cu128 image's torch crashes against the
	// lockfile's cu124 CUDA libraries.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"),
		[]byte("[project]\ndependencies = [\"torch>=2.6\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(`
[[package]]
name = "torch"
version = "2.6.0"
source = { registry = "https://download.pytorch.org/whl/cu124" }
wheels = [
    { url = "https://download.pytorch.org/whl/cu124/torch-2.6.0%2Bcu124-cp312-cp312-linux_x86_64.whl" },
]
`), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	groups := SplitGroupsByImage(nil, []InstanceGroup{{
		GPUClass: "nvidia",
		GPUMemGB: 48,
		Jobs: []*db.Job{{
			ID:         1,
			WorkingDir: dir,
			Command:    "uv run python train.py",
		}},
	}})
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}
	if groups[0].Image != "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime" {
		t.Fatalf("Image = %q, want pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime", groups[0].Image)
	}
}

func TestSplitToParallel_MultiJobGroup(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		GPUMemGB: 20, // supremum of 8, 8, 20
		DiskGB:   50,
		Image:    "cuda:12.4",
		Jobs: []*db.Job{
			{ID: 1, GPUMemGB: intPtr(20), GPUMemMaxGB: intPtr(24), MaxComputeCap: "9.0"},
			{ID: 2, GPUMemGB: intPtr(8), MaxComputeCap: "12.0"},
			{ID: 3, GPUMemGB: intPtr(8), MaxComputeCap: "12.0"},
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
	if result[0].MaxGPUMemGB != 0 {
		t.Errorf("first group MaxGPUMemGB = %d, want 0", result[0].MaxGPUMemGB)
	}
	if result[0].MaxComputeCap != "9.0" {
		t.Errorf("first group MaxComputeCap = %q, want 9.0", result[0].MaxComputeCap)
	}
	// The 8GB jobs should have their own GPUMemGB and compute-cap bound.
	for _, g := range result[1:] {
		if g.GPUMemGB != 8 {
			continue
		}
		if g.MaxComputeCap != "12.0" {
			t.Errorf("8GB group (job %d): MaxComputeCap = %q, want 12.0",
				g.Jobs[0].ID, g.MaxComputeCap)
		}
	}
}

func TestSplitToParallel_SingleJobGroup(t *testing.T) {
	group := InstanceGroup{
		GPUClass:      "RTX_3090",
		GPUMemGB:      24,
		MaxComputeCap: "9.0",
		Jobs:          []*db.Job{{ID: 1, GPUMemGB: intPtr(24)}},
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
	if result[0].MaxComputeCap != "9.0" {
		t.Errorf("MaxComputeCap = %q, want 9.0", result[0].MaxComputeCap)
	}
}

func TestSplitToParallel_EmptyInput(t *testing.T) {
	result := SplitToParallel(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 groups, got %d", len(result))
	}
}

func TestGroupMaxComputeCap_ReducesMinAcrossPersistedCaps(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, MaxComputeCap: "10.0"},
		{ID: 2, MaxComputeCap: "9.0"},
		{ID: 3, MaxComputeCap: "12.0"},
	}
	got := groupMaxComputeCap(nil, jobs)
	if got != "9.0" {
		t.Errorf("groupMaxComputeCap = %q, want %q (min of {9.0, 10.0, 12.0})", got, "9.0")
	}
}

func TestGroupMaxComputeCap_AnyMakesGroupUnbounded(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, MaxComputeCap: "9.0"},
		{ID: 2, MaxComputeCap: "any"},
	}
	got := groupMaxComputeCap(nil, jobs)
	if got != "" {
		t.Errorf("groupMaxComputeCap with explicit \"any\" = %q, want \"\" (unbounded)", got)
	}
}

func TestGroupMaxComputeCap_LazyBackfillFromMissingTorchPin(t *testing.T) {
	// No torch pin at the working dir → backfill resolves to "any", which
	// makes the group unbounded.
	jobs := []*db.Job{
		{ID: 1, MaxComputeCap: "10.0"},
		{ID: 2, MaxComputeCap: "", WorkingDir: "/nonexistent/path", GPUClass: "nvidia"},
	}
	got := groupMaxComputeCap(nil, jobs)
	if got != "" {
		t.Errorf("groupMaxComputeCap = %q, want \"\" (job 2 backfills to \"any\" → group unbounded)", got)
	}
}

func TestGroupMaxComputeCap_RefreshesStalePersistedTorchCap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(`
[[package]]
name = "torch"
version = "2.6.0"

[[package]]
name = "nvidia-cublas-cu12"
version = "12.4.5.8"
`), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	jobs := []*db.Job{
		{
			ID:            1,
			WorkingDir:    dir,
			Command:       "uv run python train.py",
			MaxComputeCap: "10.0",
			GPUClass:      "nvidia",
		},
	}

	got := groupMaxComputeCap(nil, jobs)
	if got != "9.0" {
		t.Errorf("groupMaxComputeCap = %q, want refreshed cap 9.0", got)
	}
}

// Regression: maxCUDAVersionString must compare components as integers, not
// floats — float parsing treats "12.10" as 12.1, which would silently mis-
// order future CUDA minor versions ≥ 10 below 12.8.
func TestMaxCUDAVersionString_TwoDigitMinor(t *testing.T) {
	if got := maxCUDAVersionString("12.8", "12.10"); got != "12.10" {
		t.Errorf("max(12.8, 12.10) = %q, want 12.10", got)
	}
	if got := maxCUDAVersionString("12.10", "12.8"); got != "12.10" {
		t.Errorf("max(12.10, 12.8) = %q, want 12.10", got)
	}
	if got := maxCUDAVersionString("12.10", "13.0"); got != "13.0" {
		t.Errorf("max(12.10, 13.0) = %q, want 13.0", got)
	}
}
