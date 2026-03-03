package placement

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// MonteCarloConfig controls the Monte Carlo completion-time simulation.
type MonteCarloConfig struct {
	NSamples              int     // number of Monte Carlo samples per host (default 1000)
	DefaultQueuedJobMeanS float64 // mean duration for queued jobs without predictions (default 1800)
	DefaultQueuedJobCV    float64 // coefficient of variation for queued jobs (default 0.5)
	FallbackDurationCV    float64 // CV when prediction has no bounds (default 0.3)
	Seed                  int64   // RNG seed; 0 means use a random seed
}

// DefaultMonteCarloConfig returns sensible defaults for Monte Carlo simulation.
func DefaultMonteCarloConfig() MonteCarloConfig {
	return MonteCarloConfig{
		NSamples:              1000,
		DefaultQueuedJobMeanS: 1800,
		DefaultQueuedJobCV:    0.5,
		FallbackDurationCV:    0.3,
	}
}

// CompletionEstimate summarizes the simulated completion-time distribution for a host.
type CompletionEstimate struct {
	Host               string
	MedianCompletionS  float64
	P10CompletionS     float64
	P90CompletionS     float64
	MedianQueueWaitS   float64
	MedianJobDurationS float64
	NSamples           int
}

// SimulateCompletionTimes runs Monte Carlo simulation for each eligible host
// that has a duration prediction. Returns nil if fewer than 2 hosts have predictions.
func SimulateCompletionTimes(
	scores []Score,
	predictions map[string]*JobPrediction,
	metrics map[string]*HostMetrics,
	config MonteCarloConfig,
) []CompletionEstimate {
	if config.NSamples <= 0 {
		config.NSamples = 1000
	}

	// Collect eligible hosts with duration predictions
	type hostInfo struct {
		host       string
		prediction *JobPrediction
		queueDepth int
	}
	var hosts []hostInfo
	for _, s := range scores {
		if !s.Eligible {
			continue
		}
		p := predictions[s.Host]
		if p == nil || p.DurationS == nil {
			continue
		}
		qd := queueDepthFor(metrics, s.Host)
		hosts = append(hosts, hostInfo{host: s.Host, prediction: p, queueDepth: qd})
	}

	if len(hosts) < 2 {
		return nil
	}

	// Default queued-job distribution parameters
	defaultQMu, defaultQSigma := fitLogNormalFromMean(config.DefaultQueuedJobMeanS, config.DefaultQueuedJobCV)

	seed := config.Seed
	if seed == 0 {
		seed = rand.Int63()
	}
	rng := rand.New(rand.NewSource(seed))

	estimates := make([]CompletionEstimate, 0, len(hosts))
	for _, h := range hosts {
		// Fit job duration distribution
		jobMu, jobSigma := fitJobDuration(h.prediction, config.FallbackDurationCV)

		// Fit queued-job distribution (use this host's own prediction if available)
		qMu, qSigma := defaultQMu, defaultQSigma
		if h.prediction.DurationSLower != nil && h.prediction.DurationSUpper != nil {
			qMu, qSigma = fitLogNormal(*h.prediction.DurationSLower, *h.prediction.DurationSUpper)
		}

		completionSamples := make([]float64, config.NSamples)
		waitSamples := make([]float64, config.NSamples)
		durationSamples := make([]float64, config.NSamples)

		for i := 0; i < config.NSamples; i++ {
			// Queue wait: sum of h.queueDepth independent draws
			var queueWait float64
			for j := 0; j < h.queueDepth; j++ {
				queueWait += sampleLogNormal(rng, qMu, qSigma)
			}

			jobDuration := sampleLogNormal(rng, jobMu, jobSigma)
			completionSamples[i] = queueWait + jobDuration
			waitSamples[i] = queueWait
			durationSamples[i] = jobDuration
		}

		sort.Float64s(completionSamples)
		sort.Float64s(waitSamples)
		sort.Float64s(durationSamples)

		estimates = append(estimates, CompletionEstimate{
			Host:               h.host,
			MedianCompletionS:  percentile(completionSamples, 0.5),
			P10CompletionS:     percentile(completionSamples, 0.1),
			P90CompletionS:     percentile(completionSamples, 0.9),
			MedianQueueWaitS:   percentile(waitSamples, 0.5),
			MedianJobDurationS: percentile(durationSamples, 0.5),
			NSamples:           config.NSamples,
		})
	}

	return estimates
}

// ApplyMonteCarloScoring adds a linear bonus based on Monte Carlo completion estimates.
// Fastest median completion gets +5.0, slowest gets 0.
// Removes queue-depth penalty and static perf scoring for MC-scored hosts.
func ApplyMonteCarloScoring(scores []Score, estimates []CompletionEstimate) {
	if len(estimates) < 2 {
		return
	}

	// Build lookup
	estByHost := make(map[string]*CompletionEstimate, len(estimates))
	for i := range estimates {
		estByHost[estimates[i].Host] = &estimates[i]
	}

	// Find min/max median completion
	minComp := estimates[0].MedianCompletionS
	maxComp := estimates[0].MedianCompletionS
	for _, e := range estimates[1:] {
		if e.MedianCompletionS < minComp {
			minComp = e.MedianCompletionS
		}
		if e.MedianCompletionS > maxComp {
			maxComp = e.MedianCompletionS
		}
	}

	spread := maxComp - minComp
	const maxBonus = 5.0

	for i := range scores {
		est := estByHost[scores[i].Host]
		if est == nil {
			continue
		}

		var bonus float64
		if spread > 0 {
			bonus = (1.0 - (est.MedianCompletionS-minComp)/spread) * maxBonus
		} else {
			bonus = maxBonus / 2.0
		}

		scores[i].Total += bonus

		// Remove static perf scoring since MC subsumes it
		removeStaticPerfScoring(&scores[i])

		// Remove the flat queue-depth penalty since MC models queue drain time
		removeQueueDepthPenalty(&scores[i])

		// Add MC reason
		waitMin := est.MedianQueueWaitS / 60.0
		durMin := est.MedianJobDurationS / 60.0
		compMin := est.MedianCompletionS / 60.0
		p10Min := est.P10CompletionS / 60.0
		p90Min := est.P90CompletionS / 60.0
		scores[i].Reasons = append(scores[i].Reasons,
			fmt.Sprintf("MC: ~%.0fm wait + ~%.0fm run = ~%.0fm (p10-p90: %.0f-%.0fm, %d samples)",
				waitMin, durMin, compMin, p10Min, p90Min, est.NSamples))
	}
}

// removeQueueDepthPenalty reverses the flat queue-depth penalty that was applied
// in scoreHost, since Monte Carlo models actual queue drain time.
func removeQueueDepthPenalty(s *Score) {
	if s.queuePenalty == 0 {
		return
	}
	s.Total += s.queuePenalty
	s.queuePenalty = 0

	if s.queueReason != "" {
		filtered := s.Reasons[:0]
		for _, r := range s.Reasons {
			if r != s.queueReason {
				filtered = append(filtered, r)
			}
		}
		s.Reasons = filtered
		s.queueReason = ""
	}
}

// queueDepthFor returns the queue depth for a host, or 0 if metrics are unavailable.
func queueDepthFor(metrics map[string]*HostMetrics, host string) int {
	if metrics == nil {
		return 0
	}
	if m := metrics[host]; m != nil {
		return m.QueueDepth
	}
	return 0
}

// fitLogNormal fits a log-normal distribution from p10 and p90 quantiles.
// Returns mu and sigma of the underlying normal distribution.
func fitLogNormal(p10, p90 float64) (mu, sigma float64) {
	if p10 <= 0 || p90 <= 0 {
		return 0, 0
	}
	lnP10 := math.Log(p10)
	lnP90 := math.Log(p90)
	// z_{0.9} ≈ 1.2816
	sigma = (lnP90 - lnP10) / (2 * 1.2816)
	mu = (lnP90 + lnP10) / 2
	return mu, sigma
}

// fitLogNormalFromMean fits a log-normal from a mean and coefficient of variation.
func fitLogNormalFromMean(mean, cv float64) (mu, sigma float64) {
	if mean <= 0 || cv <= 0 {
		return 0, 0
	}
	sigma2 := math.Log(1 + cv*cv)
	sigma = math.Sqrt(sigma2)
	mu = math.Log(mean) - sigma2/2
	return mu, sigma
}

// fitJobDuration returns log-normal parameters for a job's duration prediction.
// Uses p10/p90 bounds if available, otherwise falls back to mean + default CV.
func fitJobDuration(p *JobPrediction, fallbackCV float64) (mu, sigma float64) {
	if p.DurationSLower != nil && p.DurationSUpper != nil {
		return fitLogNormal(*p.DurationSLower, *p.DurationSUpper)
	}
	return fitLogNormalFromMean(*p.DurationS, fallbackCV)
}

// sampleLogNormal draws one sample from a log-normal distribution.
func sampleLogNormal(rng *rand.Rand, mu, sigma float64) float64 {
	return math.Exp(mu + sigma*rng.NormFloat64())
}

// percentile returns the p-th percentile from a sorted slice (0 ≤ p ≤ 1).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper || upper >= len(sorted) {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}
