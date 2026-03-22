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

	// Empty-class job merges into first compatible NVIDIA group (H100),
	// so we get 2 groups: {H100, h100, "", H100(running)} and {A100}.
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// Groups sorted by descending memory: H100 group (80GB) first
	if groups[0].GPUMemGB != 80 {
		t.Errorf("first group should have 80GB, got %d", groups[0].GPUMemGB)
	}
	if groups[0].GPUClass != "H100" {
		t.Errorf("first group should be H100, got %s", groups[0].GPUClass)
	}
	if len(groups[0].Jobs) != 4 {
		t.Errorf("H100 group should have 4 jobs (incl. empty-class and running-unplaced), got %d", len(groups[0].Jobs))
	}

	// A100 group
	if groups[1].GPUClass != "A100" {
		t.Errorf("second group should be A100, got %s", groups[1].GPUClass)
	}
	if groups[1].GPUMemGB != 40 {
		t.Errorf("second group should have 40GB, got %d", groups[1].GPUMemGB)
	}
}

func TestGroupByGPUSupremum_NvidiaAndEmptyMerge(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20)},
		{ID: 2, Status: db.StatusQueued, GPUClass: "", GPUMemGB: intPtr(20)},
		{ID: 3, Status: db.StatusQueued, GPUClass: "ampere", GPUMemGB: intPtr(40)},
	}

	groups := GroupByGPUSupremum(jobs)

	// "nvidia" and "" merge (both unconstrained NVIDIA family).
	// "ampere" is more specific — merges into nvidia group (nvidia subsumes ampere).
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d: %v", len(groups), groups)
	}
	if groups[0].GPUClass != "AMPERE" {
		t.Errorf("merged group should use most specific class AMPERE, got %s", groups[0].GPUClass)
	}
	if len(groups[0].Jobs) != 3 {
		t.Errorf("merged group should have 3 jobs, got %d", len(groups[0].Jobs))
	}
	if groups[0].GPUMemGB != 40 {
		t.Errorf("merged group should have 40GB (supremum), got %d", groups[0].GPUMemGB)
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
