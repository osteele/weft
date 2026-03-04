package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func intPtr(n int) *int { return &n }

func TestGroupByGPUSupremum(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusNeedsRental, GPUClass: "H100", GPUMemGB: intPtr(80)},
		{ID: 2, Status: db.StatusNeedsRental, GPUClass: "h100", GPUMemGB: intPtr(40)},
		{ID: 3, Status: db.StatusNeedsRental, GPUClass: "A100", GPUMemGB: intPtr(40)},
		{ID: 4, Status: db.StatusNeedsRental, GPUClass: "", GPUMemGB: intPtr(24)},
		{ID: 5, Status: db.StatusRunning, GPUClass: "H100", GPUMemGB: intPtr(80)}, // should be excluded
	}

	groups := GroupByGPUSupremum(jobs)

	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}

	// Groups should be sorted by descending memory
	if groups[0].GPUMemGB != 80 {
		t.Errorf("first group should have 80GB, got %d", groups[0].GPUMemGB)
	}
	if groups[0].GPUClass != "H100" {
		t.Errorf("first group should be H100, got %s", groups[0].GPUClass)
	}
	if len(groups[0].Jobs) != 2 {
		t.Errorf("H100 group should have 2 jobs, got %d", len(groups[0].Jobs))
	}

	// A100 group
	if groups[1].GPUClass != "A100" {
		t.Errorf("second group should be A100, got %s", groups[1].GPUClass)
	}
	if groups[1].GPUMemGB != 40 {
		t.Errorf("second group should have 40GB, got %d", groups[1].GPUMemGB)
	}

	// No-class group
	if groups[2].GPUMemGB != 24 {
		t.Errorf("third group should have 24GB, got %d", groups[2].GPUMemGB)
	}
}

func TestGroupByGPUSupremum_Empty(t *testing.T) {
	groups := GroupByGPUSupremum(nil)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for nil input, got %d", len(groups))
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
