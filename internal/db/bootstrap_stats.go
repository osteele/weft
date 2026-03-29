package db

import (
	"database/sql"
	"sort"
	"time"
)

// BootstrapDurations is a sorted (ascending) slice of successful bootstrap
// durations from historical instances. It supports conditional median queries
// for estimating remaining time given that a bootstrap has already taken some
// known elapsed time.
type BootstrapDurations []time.Duration

// ConditionalMedian returns the estimated remaining bootstrap time given that
// the bootstrap has already taken `elapsed`. It filters to instances that took
// at least `elapsed`, computes their median total duration, and subtracts
// elapsed. Returns (0, false) if fewer than 3 tail entries remain.
func (d BootstrapDurations) ConditionalMedian(elapsed time.Duration) (remaining time.Duration, ok bool) {
	if len(d) == 0 {
		return 0, false
	}
	// Binary search for first duration >= elapsed
	i := sort.Search(len(d), func(j int) bool {
		return d[j] >= elapsed
	})
	tail := d[i:]
	if len(tail) < 3 {
		return 0, false
	}
	median := tail[len(tail)/2]
	rem := median - elapsed
	if rem <= 0 {
		return 0, false
	}
	return rem, true
}

// BootstrapSurvival holds learned bootstrap timeout thresholds derived from
// historical instance data using survival analysis. At each time T, it
// computes the fraction of instances that were alive at T (without having
// succeeded) that eventually did bootstrap. The warn and terminate
// thresholds are the times at which this probability drops below configured
// cutoffs.
type BootstrapSurvival struct {
	Provider       string
	SampleSize     int
	WarnAfter      time.Duration      // time at which P(success) < warnCutoff
	TerminateAfter time.Duration      // time at which P(success) < terminateCutoff
	Durations      BootstrapDurations // sorted successful bootstrap durations for conditional estimates
}

// survivalConfig holds tunable parameters for a survival analysis.
type survivalConfig struct {
	BucketSecs     int     // time resolution for survival curve
	WarnCutoff     float64 // warn when P(success | alive) < this
	TermCutoff     float64 // terminate when P(success | alive) < this
	MinSamples     int     // minimum observations before using learned values
	MaxTimeSecs    int     // ignore event times beyond this (clock skew)
	MinWarnSecs    int     // floor: warn no earlier than this
	MinTermSecs    int     // floor: terminate no earlier than this
	DefaultWarnSec int     // fallback when insufficient data
	DefaultTermSec int     // fallback when insufficient data
}

// bootstrapSurvivalConfig is the configuration for bootstrap stall detection.
var bootstrapSurvivalConfig = survivalConfig{
	BucketSecs:     30,
	WarnCutoff:     0.10,
	TermCutoff:     0.03,
	MinSamples:     20,
	MaxTimeSecs:    7200,
	MinWarnSecs:    3 * 60,
	MinTermSecs:    5 * 60,
	DefaultWarnSec: 15 * 60,
	DefaultTermSec: 20 * 60,
}

// survivalObservation represents a single instance's bootstrap outcome.
type survivalObservation struct {
	eventTime int // seconds from launch to bootstrap (success) or death (failure)
	succeeded bool
}

// ComputeBootstrapSurvival queries historical launch data and returns learned
// warn/terminate thresholds for the given provider using survival analysis.
//
// The survival function answers: "given that T seconds have elapsed with no
// bootstrap progress, what fraction of instances in this situation eventually
// succeeded?" When that fraction drops below the warn/terminate cutoffs,
// we should act.
func ComputeBootstrapSurvival(database *sql.DB, provider string) (*BootstrapSurvival, error) {
	cfg := bootstrapSurvivalConfig
	obs, err := queryBootstrapObservations(database, provider)
	if err != nil {
		return nil, err
	}

	result := &BootstrapSurvival{
		Provider:       provider,
		SampleSize:     len(obs),
		WarnAfter:      time.Duration(cfg.DefaultWarnSec) * time.Second,
		TerminateAfter: time.Duration(cfg.DefaultTermSec) * time.Second,
	}

	if len(obs) < cfg.MinSamples {
		return result, nil
	}

	warnSecs, termSecs := computeSurvivalThresholds(obs, cfg)
	if warnSecs > 0 {
		result.WarnAfter = time.Duration(warnSecs) * time.Second
	}
	if termSecs > 0 {
		result.TerminateAfter = time.Duration(termSecs) * time.Second
	}

	// Extract successful durations for conditional median estimates.
	// obs is already sorted by eventTime after computeSurvivalThresholds,
	// so the filtered subset preserves sort order.
	var durations BootstrapDurations
	for _, o := range obs {
		if o.succeeded {
			durations = append(durations, time.Duration(o.eventTime)*time.Second)
		}
	}
	result.Durations = durations

	return result, nil
}

// queryBootstrapObservations fetches all terminal launches for the given
// provider and classifies each as a bootstrap success or failure.
//
// Success: the instance eventually started a job (wrapper_start is set).
//
//	event_time = wrapper_start - launched_at
//
// Failure: the instance died without starting a job.
//
//	event_time = ended_at - launched_at
func queryBootstrapObservations(database *sql.DB, provider string) ([]survivalObservation, error) {
	query := `
		WITH first_attempt AS (
			SELECT ja.launch_id, MIN(jpt.wrapper_start) AS wrapper_start
			FROM job_attempts ja
			JOIN job_phase_timings jpt ON jpt.job_id = ja.job_id
			WHERE ja.launch_id IS NOT NULL
			  AND ja.attempt_number = 1
			  AND jpt.wrapper_start IS NOT NULL
			GROUP BY ja.launch_id
		)
		SELECT
			CASE
				WHEN fa.wrapper_start IS NOT NULL
				THEN fa.wrapper_start - l.launched_at
				ELSE l.ended_at - l.launched_at
			END AS event_time,
			CASE WHEN fa.wrapper_start IS NOT NULL THEN 1 ELSE 0 END AS succeeded
		FROM launches l
		LEFT JOIN first_attempt fa ON fa.launch_id = l.id
		WHERE l.launched_at IS NOT NULL
		  AND l.status IN ('completed', 'failed')
		  AND l.provider = ?
		  AND CASE
			WHEN fa.wrapper_start IS NOT NULL
			THEN (fa.wrapper_start - l.launched_at) BETWEEN 1 AND ?
			ELSE (l.ended_at - l.launched_at) > 0
		  END
	`

	rows, err := database.Query(query, provider, bootstrapSurvivalConfig.MaxTimeSecs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var obs []survivalObservation
	for rows.Next() {
		var o survivalObservation
		if err := rows.Scan(&o.eventTime, &o.succeeded); err != nil {
			return nil, err
		}
		obs = append(obs, o)
	}
	return obs, rows.Err()
}

// computeSurvivalThresholds walks the survival curve and returns the first
// time (in seconds) at which P(success | alive at T) drops below the warn
// and terminate cutoffs. Returns 0 for either if the cutoff is never reached.
//
// Observations are sorted by eventTime so that counts can be maintained
// incrementally as the time cursor advances, giving O(N log N) overall
// instead of O(N * maxTime/bucketSize).
func computeSurvivalThresholds(obs []survivalObservation, cfg survivalConfig) (warnSecs, termSecs int) {
	if len(obs) == 0 {
		return 0, 0
	}

	// Sort by eventTime ascending so we can subtract departing observations
	// as the time cursor advances.
	sort.Slice(obs, func(i, j int) bool {
		return obs[i].eventTime < obs[j].eventTime
	})

	// Start with all observations at risk.
	atRisk := len(obs)
	willBootstrap := 0
	for _, o := range obs {
		if o.succeeded {
			willBootstrap++
		}
	}

	maxTime := obs[len(obs)-1].eventTime
	idx := 0 // next observation to subtract

	for t := 0; t <= maxTime; t += cfg.BucketSecs {
		// Subtract observations whose eventTime <= t (no longer at risk).
		for idx < len(obs) && obs[idx].eventTime <= t {
			atRisk--
			if obs[idx].succeeded {
				willBootstrap--
			}
			idx++
		}

		if atRisk == 0 {
			break
		}

		p := float64(willBootstrap) / float64(atRisk)

		if warnSecs == 0 && p < cfg.WarnCutoff && t >= cfg.MinWarnSecs {
			warnSecs = t
		}
		if termSecs == 0 && p < cfg.TermCutoff && t >= cfg.MinTermSecs {
			termSecs = t
		}

		if warnSecs > 0 && termSecs > 0 {
			break
		}
	}

	return warnSecs, termSecs
}
