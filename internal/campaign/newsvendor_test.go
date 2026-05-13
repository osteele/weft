package campaign

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/estimate"
)

func TestRecommendInstanceCountClampsToOfferGroups(t *testing.T) {
	estimates := []CostEstimate{
		newsvendorTestEstimate(2*time.Hour, 1*time.Hour, 4*time.Hour, 0.50),
		newsvendorTestEstimate(2*time.Hour, 1*time.Hour, 4*time.Hour, 0.50),
	}
	rec := RecommendInstanceCount(estimates, bidding.StrategyFast.Profile())
	if rec == nil {
		t.Fatal("expected recommendation")
	}
	if rec.Quantity != 2 {
		t.Fatalf("quantity = %d, want 2", rec.Quantity)
	}
	if rec.CriticalFractile <= 0.5 {
		t.Fatalf("critical fractile = %.3f, want fast strategy above median", rec.CriticalFractile)
	}
	if line := FormatNewsvendorRecommendation(rec); !strings.Contains(line, "launch 2 instance(s)") {
		t.Fatalf("formatted line = %q", line)
	}
}

func TestRecommendInstanceCountCheapCanChooseLowerQuantile(t *testing.T) {
	estimates := []CostEstimate{
		newsvendorTestEstimate(1*time.Hour, 30*time.Minute, 3*time.Hour, 10.0),
		newsvendorTestEstimate(1*time.Hour, 30*time.Minute, 3*time.Hour, 10.0),
		newsvendorTestEstimate(1*time.Hour, 30*time.Minute, 3*time.Hour, 10.0),
	}
	rec := RecommendInstanceCount(estimates, bidding.StrategyCheap.Profile())
	if rec == nil {
		t.Fatal("expected recommendation")
	}
	if rec.Quantity >= len(estimates) {
		t.Fatalf("quantity = %d, want cheap strategy below all groups", rec.Quantity)
	}
}

func newsvendorTestEstimate(mean, lower, upper time.Duration, rate float64) CostEstimate {
	offer := &cloud.Offer{CostPerHour: rate, GPUName: "A100"}
	return CostEstimate{
		Offer: GroupOffer{Offer: offer},
		Breakdown: estimate.Breakdown{
			Total: estimate.Estimate{Mean: mean, Lower: lower, Upper: upper},
		},
		TotalTime: mean,
	}
}
