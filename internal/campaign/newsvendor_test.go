package campaign

import (
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
	_ "modernc.org/sqlite"
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

func TestRecommendInstanceCountUsesDurationQuantiles(t *testing.T) {
	estimates := []CostEstimate{
		newsvendorQuantileEstimate(1, map[string]float64{"0.5": 3600, "0.9": 3 * 3600}),
		newsvendorQuantileEstimate(2, map[string]float64{"0.5": 3600, "0.9": 3 * 3600}),
	}
	profile := bidding.ScoreProfile{ID: "test", Weights_: bidding.StrategyWeights{Cost: 1, Time: 9}}
	rec := RecommendInstanceCount(estimates, profile)
	if rec == nil {
		t.Fatal("expected recommendation")
	}
	if rec.DemandHours != 6 {
		t.Fatalf("demand hours = %.2f, want 6.00 from p90 job quantiles", rec.DemandHours)
	}
}

func TestRecommendInstanceCountUsesHybridCampaignEffect(t *testing.T) {
	estimates := []CostEstimate{
		newsvendorQuantileEstimate(1, map[string]float64{"0.5": 3600, "0.9": 3600}),
		newsvendorQuantileEstimate(2, map[string]float64{"0.5": 3600, "0.9": 3600}),
	}
	effect := &CampaignEffect{Tau2: 0.5, SigmaW2: 0.01, NCampaigns: 4}
	for i := range estimates {
		estimates[i].CampaignEffect = effect
	}
	profile := bidding.ScoreProfile{ID: "test", Weights_: bidding.StrategyWeights{Cost: 1, Time: 9}}
	rec := RecommendInstanceCount(estimates, profile)
	if rec == nil {
		t.Fatal("expected recommendation")
	}
	if rec.DemandHours <= 2.5 {
		t.Fatalf("demand hours = %.2f, want hybrid shared-effect demand above independent p90 baseline", rec.DemandHours)
	}
}

func TestEstimateCampaignEffect(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE training_examples (launch_id INTEGER, duration_s REAL, exit_code INTEGER);
		INSERT INTO training_examples VALUES
			(1, 100, 0), (1, 400, 0),
			(2, 200, 0), (2, 800, 0),
			(3, 500, 1);
	`); err != nil {
		t.Fatalf("seed training examples: %v", err)
	}
	effect, err := EstimateCampaignEffect(database)
	if err != nil {
		t.Fatalf("EstimateCampaignEffect: %v", err)
	}
	if effect == nil {
		t.Fatal("expected campaign effect")
	}
	if effect.NCampaigns != 2 {
		t.Fatalf("NCampaigns = %d, want 2", effect.NCampaigns)
	}
	if effect.SigmaW2 <= 0 {
		t.Fatalf("SigmaW2 = %f, want positive", effect.SigmaW2)
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

func newsvendorQuantileEstimate(id int64, quantiles map[string]float64) CostEstimate {
	offer := &cloud.Offer{CostPerHour: 1, GPUName: "A100"}
	job := &db.Job{ID: id}
	return CostEstimate{
		Group: InstanceGroup{Jobs: []*db.Job{job}},
		Offer: GroupOffer{Offer: offer},
		Breakdown: estimate.Breakdown{
			Run:   estimate.Constant(time.Hour),
			Total: estimate.Constant(time.Hour),
		},
		JobDurations: map[int64]time.Duration{id: time.Hour},
		JobDurationQuantiles: map[int64]*predictor.QuantilePrediction{
			id: &predictor.QuantilePrediction{Quantiles: quantiles, Source: "command", N: 8, MeanLog: math.Log(3600)},
		},
		TotalTime: time.Hour,
	}
}
