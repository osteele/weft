package campaign

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

func TestEstimateCosts_NoPredictions(t *testing.T) {
	groupOffers := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "A100", GPUMemGB: 80, Jobs: []*db.Job{{ID: 1}, {ID: 2}, {ID: 3}}},
			Offer: &cloud.Offer{GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 1.50},
		},
	}

	estimates := EstimateCosts(groupOffers, nil, nil, nil, nil)

	if len(estimates) != 1 {
		t.Fatalf("expected 1 estimate, got %d", len(estimates))
	}

	est := estimates[0]
	if est.HasPrediction {
		t.Error("should not have prediction with nil config")
	}

	// 3 jobs * default duration + startup + provision overhead
	expectedJobTime := 3 * estimate.DefaultJobDuration.Mean
	if est.TotalTime < expectedJobTime {
		t.Errorf("TotalTime = %v, should be >= %v (3 jobs * default)", est.TotalTime, expectedJobTime)
	}

	if est.TotalCost <= 0 {
		t.Error("TotalCost should be > 0")
	}
}

func TestEstimateCosts_NilOffer(t *testing.T) {
	groupOffers := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "H100", Jobs: []*db.Job{{ID: 1}}},
			Offer: nil,
		},
	}

	estimates := EstimateCosts(groupOffers, nil, nil, nil, nil)
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

func TestBudgetFromEstimate(t *testing.T) {
	tests := []struct {
		name         string
		est          CostEstimate
		wantMinSpend int
		wantMinTime  int
		wantMaxSpend int
		wantMaxTime  int
	}{
		{
			name:         "normal estimate uses 10x multiplier",
			est:          CostEstimate{TotalTime: 2 * time.Hour, TotalCost: 5.0},
			wantMinSpend: 5000,  // $5 * 10 * 100 = 5000 cents
			wantMinTime:  72000, // 2h * 10 = 20h = 72000s
			wantMaxSpend: 5000,
			wantMaxTime:  72000,
		},
		{
			name:         "small estimate hits floor",
			est:          CostEstimate{TotalTime: 10 * time.Minute, TotalCost: 0.10},
			wantMinSpend: MinBudgetCents,               // $20 floor
			wantMinTime:  int(MinBudgetTime.Seconds()), // 8h floor
			wantMaxSpend: MinBudgetCents,
			wantMaxTime:  int(MinBudgetTime.Seconds()),
		},
		{
			name:         "zero estimate hits floor",
			est:          CostEstimate{TotalTime: 0, TotalCost: 0},
			wantMinSpend: MinBudgetCents,
			wantMinTime:  int(MinBudgetTime.Seconds()),
			wantMaxSpend: MinBudgetCents,
			wantMaxTime:  int(MinBudgetTime.Seconds()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spend, secs := BudgetFromEstimate(tt.est)
			if spend < tt.wantMinSpend {
				t.Errorf("MaxSpendCents = %d, want >= %d", spend, tt.wantMinSpend)
			}
			if spend > tt.wantMaxSpend {
				t.Errorf("MaxSpendCents = %d, want <= %d", spend, tt.wantMaxSpend)
			}
			if secs < tt.wantMinTime {
				t.Errorf("MaxTimeSeconds = %d, want >= %d", secs, tt.wantMinTime)
			}
			if secs > tt.wantMaxTime {
				t.Errorf("MaxTimeSeconds = %d, want <= %d", secs, tt.wantMaxTime)
			}
		})
	}
}

func TestFormatEstDuration(t *testing.T) {
	tests := []struct {
		dur           time.Duration
		hasPrediction bool
		wantContains  string
		wantExclude   string
	}{
		{90 * time.Minute, true, "~1h30", "est"},
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
