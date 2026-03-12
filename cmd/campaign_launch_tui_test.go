package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestLaunchModelView_ShowsPartialFailures(t *testing.T) {
	m := launchModel{
		done:          true,
		campaignID:    49,
		instanceIDs:   []int64{108},
		partialErrors: []string{"RTX3090 >=20GB: create instance: quota", "A100 >=60GB: create instance: capacity"},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"Campaign 49: launched instances: 108",
		"2 planned launch(es) failed:",
		"RTX3090",
		"A100",
		"Press Enter, Esc, or q to continue.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestLaunchModelView_ShowsCostPlaceholderWhileLoadingOffers(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "A100",
			GPUMemGB: 80,
			Jobs: []*db.Job{
				{ID: 7, Description: "train", WorkingDir: "/tmp/project-alpha"},
			},
		},
	}
	items, selected, cursor := buildItemsFromGroups(groups)
	m := launchModel{
		groups:      groups,
		items:       items,
		selected:    selected,
		cursor:      cursor,
		loading:     true,
		instanceIDs: nil,
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"── Cost Estimate",
		"Awaiting offers...",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}
