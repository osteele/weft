package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestProviderOfferRowsCountsDistinctViableMachines(t *testing.T) {
	results := []providerOfferSearchResult{{
		request: "rtx_3090",
		offers: []cloud.Offer{
			{Provider: cloud.ProviderVastai, ProviderID: "1", GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.20, MachineID: "m1"},
			{Provider: cloud.ProviderVastai, ProviderID: "2", GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.30, MachineID: "m2"},
			{Provider: cloud.ProviderVastai, ProviderID: "3", GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.40, MachineID: "m3"},
		},
	}}
	survival := map[string]float64{"1": 0.70, "2": 0.65, "3": 0.20}
	rows := providerOfferRows(results, 0.4, 2, func(offer cloud.Offer) (float64, bool) {
		return survival[offer.ProviderID], true
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.ViableOffers != 2 {
		t.Fatalf("ViableOffers = %d, want 2", row.ViableOffers)
	}
	if row.DistinctMachines == nil || *row.DistinctMachines != 2 {
		t.Fatalf("DistinctMachines = %v, want 2", row.DistinctMachines)
	}
	if row.Enough != "yes" || row.Status != "ok" {
		t.Fatalf("Enough/Status = %q/%q, want yes/ok", row.Enough, row.Status)
	}
	if row.Survival != "20-70%" {
		t.Fatalf("Survival = %q, want 20-70%%", row.Survival)
	}
}

func TestProviderOfferRowsRunpodMultiMachineNeedIsUnknown(t *testing.T) {
	results := []providerOfferSearchResult{{
		request: "rtx_4090",
		offers: []cloud.Offer{{
			Provider:    cloud.ProviderRunpod,
			ProviderID:  "NVIDIA GeForce RTX 4090",
			GPUName:     "RTX 4090",
			GPUMemGB:    24,
			CostPerHour: 0.44,
			StockStatus: "Medium",
		}},
	}}
	rows := providerOfferRows(results, 0.4, 5, func(offer cloud.Offer) (float64, bool) {
		return 0.55, true
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Enough != "unknown" {
		t.Fatalf("Enough = %q, want unknown", row.Enough)
	}
	if row.Status != "RunPod exposes stock label, not machine count" {
		t.Fatalf("Status = %q", row.Status)
	}
}

func TestProviderOfferRowsBelowSurvivalFloor(t *testing.T) {
	results := []providerOfferSearchResult{{
		request: "rtx_4090",
		offers: []cloud.Offer{{
			Provider:    cloud.ProviderRunpod,
			ProviderID:  "NVIDIA GeForce RTX 4090",
			GPUName:     "RTX 4090",
			GPUMemGB:    24,
			CostPerHour: 0.44,
			StockStatus: "Medium",
		}},
	}}
	rows := providerOfferRows(results, 0.4, 1, func(offer cloud.Offer) (float64, bool) {
		return 0.27, true
	})
	row := rows[0]
	if row.ViableOffers != 0 {
		t.Fatalf("ViableOffers = %d, want 0", row.ViableOffers)
	}
	if row.Enough != "no" || row.Status != "below survival floor" {
		t.Fatalf("Enough/Status = %q/%q, want no/below survival floor", row.Enough, row.Status)
	}
}
