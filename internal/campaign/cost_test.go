package campaign

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestEstimateCosts_NoPredictions(t *testing.T) {
	groupOffers := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "A100", GPUMemGB: 80, Jobs: []*db.Job{{ID: 1}, {ID: 2}, {ID: 3}}},
			Offer: &cloud.Offer{GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 1.50},
		},
	}

	estimates := EstimateCosts(groupOffers, nil)

	if len(estimates) != 1 {
		t.Fatalf("expected 1 estimate, got %d", len(estimates))
	}

	est := estimates[0]
	if est.HasPrediction {
		t.Error("should not have prediction with nil config")
	}

	// 3 jobs * 1hr + 10min overhead
	expectedTime := 3*DefaultJobDuration + DefaultSetupOverhead
	if est.TotalTime != expectedTime {
		t.Errorf("TotalTime = %v, want %v", est.TotalTime, expectedTime)
	}

	expectedCost := expectedTime.Hours() * 1.50
	if est.TotalCost < expectedCost-0.01 || est.TotalCost > expectedCost+0.01 {
		t.Errorf("TotalCost = %f, want ~%f", est.TotalCost, expectedCost)
	}
}

func TestEstimateCosts_NilOffer(t *testing.T) {
	groupOffers := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "H100", Jobs: []*db.Job{{ID: 1}}},
			Offer: nil,
		},
	}

	estimates := EstimateCosts(groupOffers, nil)
	if estimates[0].TotalCost != 0 {
		t.Errorf("nil offer should have 0 cost, got %f", estimates[0].TotalCost)
	}
}

func TestTotalEstimatedCostFromEstimates(t *testing.T) {
	estimates := []CostEstimate{
		{TotalCost: 3.50},
		{TotalCost: 2.00},
		{TotalCost: 0}, // nil offer group
	}

	total := TotalEstimatedCostFromEstimates(estimates)
	if total < 5.49 || total > 5.51 {
		t.Errorf("total = %f, want ~5.50", total)
	}
}

func TestFormatEstDuration(t *testing.T) {
	tests := []struct {
		dur           time.Duration
		hasPrediction bool
		wantContains  string
		wantExclude   string
	}{
		{90 * time.Minute, true, "~1h 30m", "est"},
		{2 * time.Hour, false, "(est)", ""},
		{30 * time.Second, true, "~30s", ""},
	}

	for _, tt := range tests {
		got := FormatEstDuration(tt.dur, tt.hasPrediction)
		if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
			t.Errorf("FormatEstDuration(%v, %v) = %q, want contains %q", tt.dur, tt.hasPrediction, got, tt.wantContains)
		}
		if tt.wantExclude != "" && strings.Contains(got, tt.wantExclude) {
			t.Errorf("FormatEstDuration(%v, %v) = %q, should not contain %q", tt.dur, tt.hasPrediction, got, tt.wantExclude)
		}
	}
}
