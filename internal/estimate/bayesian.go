package estimate

import (
	"math"
	"time"
)

// z90 is the z-score for the 90th percentile of the standard normal distribution.
const z90 = 1.2816

// SuffStats holds sufficient statistics for a group of log-duration observations.
type SuffStats struct {
	N        int
	SumLog   float64
	SumSqLog float64
}

// Add incorporates a new log-duration observation.
func (s *SuffStats) Add(logDur float64) {
	s.N++
	s.SumLog += logDur
	s.SumSqLog += logDur * logDur
}

// Mean returns the sample mean of log-durations.
func (s *SuffStats) Mean() float64 {
	if s.N == 0 {
		return 0
	}
	return s.SumLog / float64(s.N)
}

// Variance returns the sample variance of log-durations.
func (s *SuffStats) Variance() float64 {
	if s.N < 2 {
		return 0
	}
	n := float64(s.N)
	mean := s.SumLog / n
	return (s.SumSqLog - n*mean*mean) / (n - 1)
}

// HierarchicalModel holds the global prior and per-group sufficient stats
// for an empirical Bayes lognormal model.
type HierarchicalModel struct {
	GlobalMu    float64 // global mean of log-durations
	GlobalSigma float64 // within-group std dev of log-durations
	Tau         float64 // between-group std dev
	Groups      map[string]*SuffStats
}

// PredictiveEstimate returns the posterior predictive Estimate for a group key.
func (m *HierarchicalModel) PredictiveEstimate(group string) Estimate {
	mu, v := m.posteriorParams(group)
	predVar := v + m.GlobalSigma*m.GlobalSigma
	return lognormalEstimate(mu, predVar)
}

// posteriorParams returns the posterior mean and variance for a group.
func (m *HierarchicalModel) posteriorParams(group string) (mu, variance float64) {
	tau2 := m.Tau * m.Tau
	sigma2 := m.GlobalSigma * m.GlobalSigma

	gs, ok := m.Groups[group]
	if !ok || gs.N == 0 {
		// Unknown group: return global prior
		return m.GlobalMu, tau2
	}

	nk := float64(gs.N)
	yk := gs.Mean()

	if tau2 <= 0 {
		return m.GlobalMu, sigma2 / nk
	}

	wk := nk * tau2 / (nk*tau2 + sigma2)
	mu = wk*yk + (1-wk)*m.GlobalMu
	variance = 1.0 / (nk/sigma2 + 1.0/tau2)
	return mu, variance
}

// lognormalEstimate converts log-space parameters to a duration Estimate.
func lognormalEstimate(mu, predVar float64) Estimate {
	sdPred := math.Sqrt(predVar)
	return Estimate{
		Mean:  time.Duration(math.Exp(mu+predVar/2) * float64(time.Second)),
		Lower: time.Duration(math.Exp(mu-z90*sdPred) * float64(time.Second)),
		Upper: time.Duration(math.Exp(mu+z90*sdPred) * float64(time.Second)),
	}
}

// Fit computes the hierarchical model from group sufficient statistics.
// Returns nil if there are no observations.
func Fit(groups map[string]*SuffStats) *HierarchicalModel {
	if len(groups) == 0 {
		return nil
	}

	var global SuffStats
	for _, gs := range groups {
		global.N += gs.N
		global.SumLog += gs.SumLog
		global.SumSqLog += gs.SumSqLog
	}

	if global.N == 0 {
		return nil
	}

	globalMu := global.Mean()

	sigma2 := pooledWithinGroupVariance(groups)
	if sigma2 <= 0 {
		sigma2 = global.Variance()
	}
	if sigma2 <= 0 {
		sigma2 = 1.0 // single observation or all identical
	}

	tau2 := betweenGroupVariance(groups, globalMu, sigma2)

	return &HierarchicalModel{
		GlobalMu:    globalMu,
		GlobalSigma: math.Sqrt(sigma2),
		Tau:         math.Sqrt(tau2),
		Groups:      groups,
	}
}

// pooledWithinGroupVariance computes the pooled within-group sample variance.
func pooledWithinGroupVariance(groups map[string]*SuffStats) float64 {
	var totalSS float64
	var totalDF int
	for _, gs := range groups {
		if gs.N < 2 {
			continue
		}
		n := float64(gs.N)
		mean := gs.Mean()
		ss := gs.SumSqLog - n*mean*mean
		totalSS += ss
		totalDF += gs.N - 1
	}
	if totalDF <= 0 {
		return 0
	}
	return totalSS / float64(totalDF)
}

// betweenGroupVariance estimates tau^2 using method of moments.
func betweenGroupVariance(groups map[string]*SuffStats, globalMu, sigma2 float64) float64 {
	k := 0
	var ssb float64
	for _, gs := range groups {
		if gs.N == 0 {
			continue
		}
		diff := gs.Mean() - globalMu
		ssb += diff * diff
		k++
	}
	if k < 2 {
		return 0
	}
	var invNSum float64
	for _, gs := range groups {
		if gs.N == 0 {
			continue
		}
		invNSum += 1.0 / float64(gs.N)
	}
	nHarmonic := float64(k) / invNSum

	msb := ssb / float64(k-1)
	tau2 := msb - sigma2/nHarmonic
	if tau2 < 0 {
		tau2 = 0
	}
	return tau2
}
