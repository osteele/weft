package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
)

func TestFormatLivePlacementDiagnosisShowsCounterfactualAndScope(t *testing.T) {
	diagnosis := campaign.PlacementDiagnosis{
		GPURequest: "L4",
		Exact: campaign.PlacementProbe{
			RawCount: 1,
			Detail:   "1 offer rejected (predicted survival 27% < 40%)",
		},
		Counterfactuals: []campaign.PlacementCounterfactual{{
			Constraint: "survival floor",
			Relaxation: "ignore the 40% predicted-survival floor",
			Result: campaign.PlacementProbe{
				Offer: &cloud.Offer{
					Provider: cloud.ProviderRunpod, GPUName: "L4",
					GPUMemGB: 24, CostPerHour: 0.34,
				},
				Survival: 0.27,
				RawCount: 1,
			},
		}},
		TestedConstraints: []string{"survival floor", "provider pin"},
	}

	got := formatLivePlacementDiagnosis(diagnosis)
	for _, want := range []string{
		"Exact: 1 offer rejected (predicted survival 27% < 40%)",
		"survival floor: ignore the 40% predicted-survival floor -> would admit RunPod L4, 24GB, $0.34/hr, predicted survival 27%",
		"Scope: kept L4 fixed; tested survival floor, provider pin.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnosis =\n%s\nwant substring %q", got, want)
		}
	}
	if strings.Contains(got, "required 40%") {
		t.Fatalf("diagnosis retained redundant wording: %s", got)
	}
}
