package orchestration

import (
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestBuildMoveGroupProgressLabels_UsesOrdinalAndAnchorJobID(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "NVIDIA",
			Jobs: []*db.Job{
				{ID: 1069},
			},
		},
		{
			GPUClass: "NVIDIA",
			Jobs: []*db.Job{
				{ID: 1063},
				{ID: 1090},
			},
		},
	}

	labels := buildMoveGroupProgressLabels(groups)
	if got := moveGroupProgressLabel(groups[0], labels); got != "[1/2 wj1069]" {
		t.Fatalf("label for group 1 = %q, want %q", got, "[1/2 wj1069]")
	}
	if got := moveGroupProgressLabel(groups[1], labels); got != "[2/2 wj1063]" {
		t.Fatalf("label for group 2 = %q, want %q", got, "[2/2 wj1063]")
	}
}

func TestBuildMoveGroupProgressLabels_FallsBackToOrdinalWhenNoJobIDs(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "NVIDIA",
			Jobs:     []*db.Job{{ID: 0}, nil},
		},
	}

	labels := buildMoveGroupProgressLabels(groups)
	if got := moveGroupProgressLabel(groups[0], labels); got != "[1/1]" {
		t.Fatalf("label = %q, want %q", got, "[1/1]")
	}
}
