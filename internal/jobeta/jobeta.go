// Package jobeta estimates the remaining wall-clock duration of a running
// job. The estimator blends a prior (from the job's placement-time prediction
// or a default) with live progress reported by the in-cluster agent.
//
// This is the same estimator used by `weft tui`'s grouped-status list; it
// lives in its own package so other UIs (e.g. `weft dashboard`) can reuse it
// without depending on the terminal TUI package.
package jobeta

import (
	"database/sql"
	"math"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

const (
	// MaxRunningRemainder caps how far into the future we extrapolate. Past
	// this, the estimate is essentially noise and we don't want to mislead
	// the UI into thinking we have signal.
	MaxRunningRemainder = 4 * time.Hour

	progressFreshnessWindow = 3 * time.Minute
	fallbackLowerScale      = 0.5
	fallbackUpperScale      = 2.0
	normalP90Z              = 1.2815515655446004
)

// EstimateRunningJobRemaining returns the estimated wall-clock remainder for
// a running job. The second return is false when the job is not in a state
// where an ETA is meaningful (e.g. it's queued, completed, or has no start
// time).
//
// launchLiveByID may be nil; without it, the estimator falls back to the
// prior distribution alone.
func EstimateRunningJobRemaining(job *db.Job, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) (estimate.Estimate, bool) {
	if job == nil {
		return estimate.Estimate{}, false
	}
	if status := job.EffectiveStatus(); status != db.StatusRunning && status != db.StatusStarting {
		return estimate.Estimate{}, false
	}

	elapsed := jobElapsed(job, now)
	prior := priorEstimate(job)
	priorRemaining := truncatedLogNormalRemaining(prior, elapsed)

	live := liveStateForJob(job, launchLiveByID)
	progressRemaining, hasProgress := progressBasedRemaining(elapsed, live)
	if !hasProgress {
		return capRemaining(priorRemaining), true
	}

	weight := progressBlendWeight(live, now)
	blendedMean := time.Duration((1.0-weight)*float64(priorRemaining.Mean) + weight*float64(progressRemaining))
	var blended estimate.Estimate
	if priorRemaining.Mean > 0 {
		ratio := float64(blendedMean) / float64(priorRemaining.Mean)
		blended = estimate.Estimate{
			Mean:  blendedMean,
			Lower: time.Duration(float64(priorRemaining.Lower) * ratio),
			Upper: time.Duration(float64(priorRemaining.Upper) * ratio),
		}
	} else {
		blended = estimate.Constant(blendedMean)
	}
	return capRemaining(blended), true
}

// JobProgressFraction returns a [0,1) fraction representing how far through
// the job we are, based on elapsed and estimated remaining. Returns 0 when
// the ETA is not estimable.
func JobProgressFraction(job *db.Job, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) float64 {
	if job == nil {
		return 0
	}
	remaining, ok := EstimateRunningJobRemaining(job, launchLiveByID, now)
	if !ok || remaining.Mean <= 0 {
		return 0
	}
	elapsed := jobElapsed(job, now)
	total := elapsed + remaining.Mean
	if total <= 0 {
		return 0
	}
	frac := float64(elapsed) / float64(total)
	if frac < 0 {
		return 0
	}
	if frac > 0.99 {
		frac = 0.99
	}
	return frac
}

func jobElapsed(job *db.Job, now time.Time) time.Duration {
	if job == nil || job.StartTime <= 0 {
		return 0
	}
	elapsed := now.Unix() - job.StartTime
	if elapsed <= 0 {
		return 0
	}
	return time.Duration(elapsed) * time.Second
}

func priorEstimate(job *db.Job) estimate.Estimate {
	fallback := estimate.DefaultJobDuration
	if job == nil || job.PlacementMeta == nil || job.PlacementMeta.PredictedDurationS == nil {
		return fallback
	}
	meanSec := *job.PlacementMeta.PredictedDurationS
	if !(meanSec > 0) {
		return fallback
	}
	lowerSec := meanSec * fallbackLowerScale
	upperSec := meanSec * fallbackUpperScale
	return estimate.FromSeconds(meanSec, lowerSec, upperSec)
}

func truncatedLogNormalRemaining(prior estimate.Estimate, elapsed time.Duration) estimate.Estimate {
	mu, sigma, ok := fitLogNormalFromEstimate(prior)
	if !ok {
		mean := prior.Mean
		if mean <= 0 {
			mean = estimate.DefaultJobDuration.Mean
		}
		remaining := mean - elapsed
		if remaining < 0 {
			remaining = 0
		}
		return estimate.Constant(remaining)
	}

	t := elapsed
	if t < time.Second {
		t = time.Second
	}
	lnT := math.Log(float64(t) / float64(time.Second))

	calcRemaining := func(m float64) time.Duration {
		surv := normalCDF((m - lnT) / sigma)
		if surv <= 1e-9 {
			return 0
		}
		tail := math.Exp(m+0.5*sigma*sigma) * normalCDF((m+sigma*sigma-lnT)/sigma)
		condTotalSec := tail / surv
		remainingSec := condTotalSec - float64(t)/float64(time.Second)
		if remainingSec <= 0 {
			return 0
		}
		return time.Duration(remainingSec * float64(time.Second))
	}

	return estimate.Estimate{
		Mean:  calcRemaining(mu),
		Lower: calcRemaining(mu - sigma),
		Upper: calcRemaining(mu + sigma),
	}
}

func fitLogNormalFromEstimate(prior estimate.Estimate) (mu, sigma float64, ok bool) {
	lowerSec := prior.Lower.Seconds()
	upperSec := prior.Upper.Seconds()
	meanSec := prior.Mean.Seconds()

	switch {
	case lowerSec > 0 && upperSec > lowerSec:
		sigma = (math.Log(upperSec) - math.Log(lowerSec)) / (2 * normalP90Z)
		if !(sigma > 0) {
			return 0, 0, false
		}
		mu = math.Log(lowerSec) + normalP90Z*sigma
		return mu, sigma, true
	case meanSec > 0:
		sigma = 0.6
		mu = math.Log(meanSec) - 0.5*sigma*sigma
		return mu, sigma, true
	default:
		return 0, 0, false
	}
}

func normalCDF(z float64) float64 {
	return 0.5 * (1.0 + math.Erf(z/math.Sqrt2))
}

func liveStateForJob(job *db.Job, launchLiveByID map[int64]*db.LaunchLiveState) *db.LaunchLiveState {
	if job == nil || job.LaunchID == nil || launchLiveByID == nil {
		return nil
	}
	live := launchLiveByID[*job.LaunchID]
	if live == nil || live.JobProgressID != job.ID {
		return nil
	}
	return live
}

func progressBasedRemaining(elapsed time.Duration, live *db.LaunchLiveState) (time.Duration, bool) {
	if live == nil || elapsed <= 0 {
		return 0, false
	}
	pct := live.JobProgressPct
	if pct <= 0 || pct >= 100 {
		return 0, false
	}
	remaining := time.Duration(float64(elapsed) * float64(100-pct) / float64(pct))
	if remaining < 0 {
		return 0, false
	}
	return remaining, true
}

func progressBlendWeight(live *db.LaunchLiveState, now time.Time) float64 {
	if live == nil {
		return 0.0
	}
	pct := float64(live.JobProgressPct)
	weight := 0.2
	if pct > 10 {
		weight += 0.6 * math.Min(1.0, (pct-10.0)/70.0)
	}

	if live.UpdatedAt <= 0 {
		return weight * 0.6
	}
	age := now.Unix() - live.UpdatedAt
	if age <= 0 {
		return weight
	}
	ageDur := time.Duration(age) * time.Second
	if ageDur > progressFreshnessWindow {
		return weight * 0.4
	}
	return weight
}

func capRemaining(e estimate.Estimate) estimate.Estimate {
	cap := func(d time.Duration) time.Duration {
		if d <= 0 {
			return 0
		}
		if d > MaxRunningRemainder {
			return MaxRunningRemainder
		}
		return d
	}
	return estimate.Estimate{
		Mean:  cap(e.Mean),
		Lower: cap(e.Lower),
		Upper: cap(e.Upper),
	}
}

// LoadLaunchLiveStatesForJobs loads the per-launch live-state rows the
// estimator consults for live progress signals. Pass the jobs you care about
// (the function pulls just the launches those jobs reference). Returns a map
// indexed by launch ID; safe to pass nil where the caller doesn't have access
// to live state.
func LoadLaunchLiveStatesForJobs(database *sql.DB, jobs []*db.Job) (map[int64]*db.LaunchLiveState, error) {
	seen := map[int64]struct{}{}
	ids := []int64{}
	for _, j := range jobs {
		if j == nil || j.LaunchID == nil || *j.LaunchID <= 0 {
			continue
		}
		id := *j.LaunchID
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return map[int64]*db.LaunchLiveState{}, nil
	}
	return db.GetLaunchLiveStates(database, ids)
}
