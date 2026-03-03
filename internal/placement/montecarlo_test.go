package placement

import (
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestFitLogNormal_P10P90(t *testing.T) {
	// Fit from p10=300, p90=1200, then verify recovered quantiles
	p10, p90 := 300.0, 1200.0
	mu, sigma := fitLogNormal(p10, p90)

	// The median of a log-normal is exp(mu)
	median := math.Exp(mu)
	expectedMedian := math.Sqrt(p10 * p90) // geometric mean of p10/p90
	if math.Abs(median-expectedMedian)/expectedMedian > 0.01 {
		t.Errorf("median = %.1f, expected ~%.1f", median, expectedMedian)
	}

	// Verify by sampling: p10 and p90 of samples should be close to inputs
	config := MonteCarloConfig{NSamples: 50000, Seed: 42}
	rng := rand.New(rand.NewSource(config.Seed))
	samples := make([]float64, config.NSamples)
	for i := range samples {
		samples[i] = sampleLogNormal(rng, mu, sigma)
	}
	sort.Float64s(samples)

	gotP10 := percentile(samples, 0.1)
	gotP90 := percentile(samples, 0.9)
	if math.Abs(gotP10-p10)/p10 > 0.05 {
		t.Errorf("sampled p10 = %.1f, expected ~%.1f (5%% tolerance)", gotP10, p10)
	}
	if math.Abs(gotP90-p90)/p90 > 0.05 {
		t.Errorf("sampled p90 = %.1f, expected ~%.1f (5%% tolerance)", gotP90, p90)
	}
}

func TestSimulateCompletionTimes_NoQueueDepth(t *testing.T) {
	scores := []Score{
		{Host: "fast", Eligible: true},
		{Host: "slow", Eligible: true},
	}
	fastDur, fastLo, fastHi := 600.0, 400.0, 900.0
	slowDur, slowLo, slowHi := 1200.0, 800.0, 1800.0
	predictions := map[string]*JobPrediction{
		"fast": {DurationS: &fastDur, DurationSLower: &fastLo, DurationSUpper: &fastHi},
		"slow": {DurationS: &slowDur, DurationSLower: &slowLo, DurationSUpper: &slowHi},
	}

	config := MonteCarloConfig{NSamples: 5000, Seed: 42}
	estimates := SimulateCompletionTimes(scores, predictions, nil, config)

	if len(estimates) != 2 {
		t.Fatalf("expected 2 estimates, got %d", len(estimates))
	}

	// With no queue, completion ≈ job duration
	for _, e := range estimates {
		if e.MedianQueueWaitS > 1.0 {
			t.Errorf("host %s: queue wait should be ~0 with no queue, got %.1fs", e.Host, e.MedianQueueWaitS)
		}
	}

	fastEst := findEstimate(estimates, "fast")
	slowEst := findEstimate(estimates, "slow")
	if fastEst.MedianCompletionS >= slowEst.MedianCompletionS {
		t.Errorf("fast (%.0fs) should complete before slow (%.0fs)",
			fastEst.MedianCompletionS, slowEst.MedianCompletionS)
	}
}

func TestSimulateCompletionTimes_WithQueueDepth(t *testing.T) {
	scores := []Score{
		{Host: "empty", Eligible: true},
		{Host: "busy", Eligible: true},
	}
	dur, lo, hi := 600.0, 400.0, 900.0
	predictions := map[string]*JobPrediction{
		"empty": {DurationS: &dur, DurationSLower: &lo, DurationSUpper: &hi},
		"busy":  {DurationS: &dur, DurationSLower: &lo, DurationSUpper: &hi},
	}
	metrics := map[string]*HostMetrics{
		"empty": {QueueDepth: 0},
		"busy":  {QueueDepth: 5},
	}

	config := MonteCarloConfig{NSamples: 5000, Seed: 42}
	estimates := SimulateCompletionTimes(scores, predictions, metrics, config)

	emptyEst := findEstimate(estimates, "empty")
	busyEst := findEstimate(estimates, "busy")

	// Busy host should have longer completion time due to queue
	if busyEst.MedianCompletionS <= emptyEst.MedianCompletionS {
		t.Errorf("busy (%.0fs) should take longer than empty (%.0fs)",
			busyEst.MedianCompletionS, emptyEst.MedianCompletionS)
	}
	// Busy host should have significant queue wait
	if busyEst.MedianQueueWaitS < 100 {
		t.Errorf("busy host with 5 queued jobs should have >100s queue wait, got %.0fs",
			busyEst.MedianQueueWaitS)
	}
}

func TestSimulateCompletionTimes_NoPredictions(t *testing.T) {
	scores := []Score{
		{Host: "a", Eligible: true},
		{Host: "b", Eligible: true},
	}
	predictions := map[string]*JobPrediction{}

	config := DefaultMonteCarloConfig()
	config.Seed = 42
	estimates := SimulateCompletionTimes(scores, predictions, nil, config)

	if estimates != nil {
		t.Errorf("expected nil with no predictions, got %d estimates", len(estimates))
	}
}

func TestApplyMonteCarloScoring_FastestHostWins(t *testing.T) {
	scores := []Score{
		{Host: "fast", Eligible: true, Total: 10},
		{Host: "slow", Eligible: true, Total: 10},
	}
	estimates := []CompletionEstimate{
		{Host: "fast", MedianCompletionS: 600, P10CompletionS: 400, P90CompletionS: 900, MedianQueueWaitS: 0, MedianJobDurationS: 600, NSamples: 1000},
		{Host: "slow", MedianCompletionS: 1200, P10CompletionS: 800, P90CompletionS: 1800, MedianQueueWaitS: 0, MedianJobDurationS: 1200, NSamples: 1000},
	}

	ApplyMonteCarloScoring(scores, estimates)

	fast := findScore(scores, "fast")
	slow := findScore(scores, "slow")

	if fast.Total <= slow.Total {
		t.Errorf("fast (%.2f) should score higher than slow (%.2f)", fast.Total, slow.Total)
	}

	// Check that MC reason is present
	hasMCReason := false
	for _, r := range fast.Reasons {
		if strings.HasPrefix(r, "MC:") {
			hasMCReason = true
		}
	}
	if !hasMCReason {
		t.Errorf("fast host should have MC reason, got: %v", fast.Reasons)
	}
}

func TestApplyMonteCarloScoring_QueueOverridesDuration(t *testing.T) {
	// Fast host with deep queue should lose to slow host with empty queue
	scores := []Score{
		{Host: "fast-busy", Eligible: true, Total: 8.5, Reasons: []string{"3 jobs queued"}, queuePenalty: 1.5, queueReason: "3 jobs queued"},
		{Host: "slow-idle", Eligible: true, Total: 10},
	}

	estimates := []CompletionEstimate{
		{Host: "fast-busy", MedianCompletionS: 3600, P10CompletionS: 2400, P90CompletionS: 5400, MedianQueueWaitS: 3000, MedianJobDurationS: 600, NSamples: 1000},
		{Host: "slow-idle", MedianCompletionS: 1200, P10CompletionS: 800, P90CompletionS: 1800, MedianQueueWaitS: 0, MedianJobDurationS: 1200, NSamples: 1000},
	}

	ApplyMonteCarloScoring(scores, estimates)

	fastBusy := findScore(scores, "fast-busy")
	slowIdle := findScore(scores, "slow-idle")

	if slowIdle.Total <= fastBusy.Total {
		t.Errorf("slow-idle (%.2f) should beat fast-busy (%.2f) due to queue",
			slowIdle.Total, fastBusy.Total)
	}
}

func TestMonteCarlo_FallbackWhenNoPredictions(t *testing.T) {
	db := setupTestDB(t)

	// No predictor — should use deterministic scoring, same as ScoreHosts
	scoresWithPredictor, err := ScoreHostsWithPredictor(db, Constraints{Command: "test"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scoresWithout, err := ScoreHosts(db, Constraints{})
	if err != nil {
		t.Fatal(err)
	}

	for i := range scoresWithPredictor {
		if scoresWithPredictor[i].Total != scoresWithout[i].Total {
			t.Errorf("host %s: nil predictor %.2f != no-predictor %.2f",
				scoresWithPredictor[i].Host, scoresWithPredictor[i].Total, scoresWithout[i].Total)
		}
	}
}

func TestMonteCarlo_IntegrationThroughScoreHostsWithPredictor(t *testing.T) {
	db := setupTestDB(t)

	// Set up predictions with bounds so MC will activate
	predict := func(host string) *JobPrediction {
		switch host {
		case "cool100":
			d, lo, hi := 600.0, 400.0, 900.0
			return &JobPrediction{DurationS: &d, DurationSLower: &lo, DurationSUpper: &hi}
		case "cool30":
			d, lo, hi := 1200.0, 800.0, 1800.0
			return &JobPrediction{DurationS: &d, DurationSLower: &lo, DurationSUpper: &hi}
		case "studio":
			d, lo, hi := 800.0, 600.0, 1100.0
			return &JobPrediction{DurationS: &d, DurationSLower: &lo, DurationSUpper: &hi}
		}
		return nil
	}

	metrics := map[string]*HostMetrics{
		"cool100": {QueueDepth: 0},
		"cool30":  {QueueDepth: 3},
		"studio":  {QueueDepth: 0},
	}

	scores, err := ScoreHostsWithPredictor(db, Constraints{Command: "test"}, metrics, predict)
	if err != nil {
		t.Fatal(err)
	}

	// cool100 (600s, no queue) should beat cool30 (1200s, 3 queued)
	cool100 := findScore(scores, "cool100")
	cool30 := findScore(scores, "cool30")
	if cool100.Total <= cool30.Total {
		t.Errorf("cool100 (%.2f) should beat cool30 (%.2f) — faster + no queue",
			cool100.Total, cool30.Total)
	}

	// Verify MC reasons are present (not deterministic "predicted" reasons)
	hasMC := false
	for _, r := range cool100.Reasons {
		if strings.HasPrefix(r, "MC:") {
			hasMC = true
		}
	}
	if !hasMC {
		t.Errorf("expected MC reason on cool100, got: %v", cool100.Reasons)
	}
}

// helpers

func findEstimate(estimates []CompletionEstimate, host string) CompletionEstimate {
	for _, e := range estimates {
		if e.Host == host {
			return e
		}
	}
	return CompletionEstimate{}
}
