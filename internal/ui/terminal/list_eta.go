package terminal

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

const etaLaunchSetupOverhead = 4 * time.Minute
const etaMaxRunningRemainder = 4 * time.Hour
const etaUnknownDuration = -1 * time.Second
const etaProgressFreshnessWindow = 3 * time.Minute
const etaFallbackLowerScale = 0.5
const etaFallbackUpperScale = 2.0
const normalP90Z = 1.2815515655446004

type etaResult struct {
	ETACurrent     time.Duration // etaUnknownDuration when unavailable
	ETAWithNewInst time.Duration // etaUnknownDuration when unavailable
	HasQueued      bool
}

type etaBucket struct {
	runningRemaining []time.Duration
	runningSlots     int
	queuedCount      int
}

func computeGroupedETA(jobs []*db.Job, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) etaResult {
	buckets := make(map[string]*etaBucket)
	hasQueued := false
	totalQueued := 0
	defaultDur := estimate.DefaultJobDuration.Mean

	for _, job := range jobs {
		if job == nil {
			continue
		}
		status := job.EffectiveStatus()
		if status != db.StatusRunning && status != db.StatusStarting && status != db.StatusQueued && status != db.StatusPendingPlacement {
			continue
		}
		key := etaBucketKey(job)
		bucket := buckets[key]
		if bucket == nil {
			bucket = &etaBucket{}
			buckets[key] = bucket
		}

		switch status {
		case db.StatusQueued, db.StatusPendingPlacement:
			bucket.queuedCount++
			totalQueued++
			hasQueued = true
		case db.StatusRunning, db.StatusStarting:
			bucket.runningRemaining = append(bucket.runningRemaining, runningJobRemaining(job, launchLiveByID, now))
			if job.LaunchID != nil && *job.LaunchID > 0 {
				// De-duplicate launch slots later.
				bucket.runningSlots++
			} else {
				// Running rows without launch IDs still occupy a slot.
				bucket.runningSlots++
			}
		}
	}

	// runningSlots above over-counts when multiple jobs share a launch; fix it.
	for key := range buckets {
		buckets[key].runningSlots = bucketRunningSlots(key, jobs)
	}

	var current time.Duration = etaUnknownDuration
	perBucketCurrent := make(map[string]time.Duration, len(buckets))
	for key, bucket := range buckets {
		eta := bucketETA(bucket, defaultDur, false)
		perBucketCurrent[key] = eta
		if eta > current {
			current = eta
		}
	}

	withNew := etaUnknownDuration
	if totalQueued >= 2 {
		keys := make([]string, 0, len(buckets))
		for key, bucket := range buckets {
			if bucket.queuedCount > 0 {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			candidate := etaUnknownDuration
			for k, eta := range perBucketCurrent {
				compare := eta
				if k == key {
					compare = bucketETA(buckets[k], defaultDur, true)
				}
				if compare > candidate {
					candidate = compare
				}
			}
			if candidate > 0 && (withNew == etaUnknownDuration || candidate < withNew) {
				withNew = candidate
			}
		}
	}

	return etaResult{
		ETACurrent:     current,
		ETAWithNewInst: withNew,
		HasQueued:      hasQueued,
	}
}

func formatETALine(result etaResult) string {
	if result.ETACurrent <= 0 {
		return ""
	}
	line := "ETA: " + formatETAApprox(result.ETACurrent)
	if result.HasQueued {
		line += "  ·  with +1 instance"
		if result.ETAWithNewInst > 0 && result.ETAWithNewInst < result.ETACurrent {
			line += ": " + formatETAApprox(result.ETAWithNewInst)
		}
	}
	return line
}

func formatETAApprox(d time.Duration) string {
	switch {
	case d <= 0:
		return "unknown"
	case d < time.Minute:
		return "<1m"
	case d > 24*time.Hour:
		return ">24h"
	case d < time.Hour:
		return fmt.Sprintf("~%dm", int(math.Round(d.Minutes())))
	default:
		totalMinutes := int(math.Round(d.Minutes()))
		hours := totalMinutes / 60
		minutes := totalMinutes % 60
		if minutes == 0 {
			return fmt.Sprintf("~%dh", hours)
		}
		return fmt.Sprintf("~%dh %dm", hours, minutes)
	}
}

func etaBucketKey(job *db.Job) string {
	if job == nil {
		return "unknown"
	}
	key := strings.TrimSpace(job.GPUClass)
	if key != "" && job.GPUMemGB != nil && *job.GPUMemGB > 0 {
		key = fmt.Sprintf("%s:%d", key, *job.GPUMemGB)
	}
	if key == "" {
		key = "unknown"
	}
	return key
}

func bucketRunningSlots(bucketKey string, jobs []*db.Job) int {
	launchIDs := map[int64]bool{}
	slotsWithoutLaunch := 0
	for _, job := range jobs {
		if job == nil || etaBucketKey(job) != bucketKey {
			continue
		}
		status := job.EffectiveStatus()
		if status != db.StatusRunning && status != db.StatusStarting {
			continue
		}
		if job.LaunchID != nil && *job.LaunchID > 0 {
			launchIDs[*job.LaunchID] = true
		} else {
			slotsWithoutLaunch++
		}
	}
	return len(launchIDs) + slotsWithoutLaunch
}

func runningJobRemaining(job *db.Job, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) time.Duration {
	remaining, ok := estimateRunningJobRemaining(job, launchLiveByID, now)
	if ok {
		return remaining
	}
	if job == nil {
		return 0
	}
	if job.StartTime <= 0 {
		return min(15*time.Minute, estimate.DefaultJobDuration.Mean)
	}
	elapsed := now.Unix() - job.StartTime
	if elapsed < 0 {
		elapsed = 0
	}
	elapsedDur := time.Duration(elapsed) * time.Second
	if elapsedDur > etaMaxRunningRemainder {
		elapsedDur = etaMaxRunningRemainder
	}

	if job.LaunchID != nil && launchLiveByID != nil {
		if live := launchLiveByID[*job.LaunchID]; live != nil && live.JobProgressID == job.ID && live.JobProgressPct > 0 && live.JobProgressPct < 100 {
			remaining := time.Duration(float64(elapsedDur) * float64(100-live.JobProgressPct) / float64(live.JobProgressPct))
			if remaining < 0 {
				return 0
			}
			if remaining > etaMaxRunningRemainder {
				return etaMaxRunningRemainder
			}
			return remaining
		}
	}

	defaultDur := estimate.DefaultJobDuration.Mean
	if elapsedDur >= defaultDur {
		return 0
	}
	return defaultDur - elapsedDur
}

func estimateRunningJobRemaining(job *db.Job, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) (time.Duration, bool) {
	if job == nil {
		return 0, false
	}
	if status := job.EffectiveStatus(); status != db.StatusRunning && status != db.StatusStarting {
		return 0, false
	}

	elapsed := runningJobElapsed(job, now)
	prior := runningJobPriorEstimate(job)
	priorRemaining := truncatedLogNormalRemaining(prior, elapsed)

	live := launchLiveStateForJob(job, launchLiveByID)
	progressRemaining, hasProgress := progressBasedRemaining(elapsed, live)
	if !hasProgress {
		return capRunningRemaining(priorRemaining), true
	}

	weight := progressBlendWeight(live, now)
	blended := time.Duration((1.0-weight)*float64(priorRemaining) + weight*float64(progressRemaining))
	return capRunningRemaining(blended), true
}

func runningJobElapsed(job *db.Job, now time.Time) time.Duration {
	if job == nil || job.StartTime <= 0 {
		return 0
	}
	elapsed := now.Unix() - job.StartTime
	if elapsed <= 0 {
		return 0
	}
	return time.Duration(elapsed) * time.Second
}

func runningJobPriorEstimate(job *db.Job) estimate.Estimate {
	fallback := estimate.DefaultJobDuration
	if job == nil || job.PlacementMeta == nil || job.PlacementMeta.PredictedDurationS == nil {
		return fallback
	}
	meanSec := *job.PlacementMeta.PredictedDurationS
	if !(meanSec > 0) {
		return fallback
	}
	lowerSec := meanSec * etaFallbackLowerScale
	upperSec := meanSec * etaFallbackUpperScale
	return estimate.FromSeconds(meanSec, lowerSec, upperSec)
}

func truncatedLogNormalRemaining(prior estimate.Estimate, elapsed time.Duration) time.Duration {
	mu, sigma, ok := fitLogNormalFromEstimate(prior)
	if !ok {
		mean := prior.Mean
		if mean <= 0 {
			mean = estimate.DefaultJobDuration.Mean
		}
		remaining := mean - elapsed
		if remaining < 0 {
			return 0
		}
		return remaining
	}

	t := elapsed
	if t < time.Second {
		t = time.Second
	}
	lnT := math.Log(float64(t) / float64(time.Second))

	surv := normalCDF((mu - lnT) / sigma)
	if surv <= 1e-9 {
		return 0
	}

	tail := math.Exp(mu+0.5*sigma*sigma) * normalCDF((mu+sigma*sigma-lnT)/sigma)
	condTotalSec := tail / surv
	remainingSec := condTotalSec - float64(t)/float64(time.Second)
	if remainingSec <= 0 {
		return 0
	}
	return time.Duration(remainingSec * float64(time.Second))
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

func launchLiveStateForJob(job *db.Job, launchLiveByID map[int64]*db.LaunchLiveState) *db.LaunchLiveState {
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

	// Older progress updates are still useful but less trustworthy.
	if live.UpdatedAt <= 0 {
		return weight * 0.6
	}
	age := now.Unix() - live.UpdatedAt
	if age <= 0 {
		return weight
	}
	ageDur := time.Duration(age) * time.Second
	if ageDur > etaProgressFreshnessWindow {
		return weight * 0.4
	}
	return weight
}

func capRunningRemaining(remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return 0
	}
	if remaining > etaMaxRunningRemainder {
		return etaMaxRunningRemainder
	}
	return remaining
}

func bucketETA(bucket *etaBucket, avgJobDuration time.Duration, addOneInstance bool) time.Duration {
	if bucket == nil {
		return etaUnknownDuration
	}
	maxRemaining := time.Duration(0)
	for _, d := range bucket.runningRemaining {
		if d > maxRemaining {
			maxRemaining = d
		}
	}

	if bucket.queuedCount == 0 {
		if maxRemaining > 0 {
			return maxRemaining
		}
		return etaUnknownDuration
	}

	slots := bucket.runningSlots
	if addOneInstance {
		slots++
	}
	if slots <= 0 {
		eta := time.Duration(bucket.queuedCount) * avgJobDuration
		if addOneInstance {
			eta += etaLaunchSetupOverhead
		}
		return eta
	}

	waves := int(math.Ceil(float64(bucket.queuedCount) / float64(slots)))
	eta := maxRemaining + time.Duration(waves)*avgJobDuration
	if addOneInstance {
		eta += etaLaunchSetupOverhead
	}
	return eta
}
