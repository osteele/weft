package db

import (
	"database/sql"
	"sort"
	"time"
)

// BootstrapSurvival holds learned bootstrap timeout thresholds derived from
// historical instance data using survival analysis. At each time T, it
// computes the fraction of instances that were alive at T (without having
// bootstrapped) that eventually did bootstrap. The warn and terminate
// thresholds are the times at which this probability drops below configured
// cutoffs.
type BootstrapSurvival struct {
	Provider       string
	SampleSize     int
	WarnAfter      time.Duration // time at which P(success) < warnCutoff
	TerminateAfter time.Duration // time at which P(success) < terminateCutoff
}

// Survival analysis parameters.
const (
	survivalBucketSecs  = 30      // time resolution for survival curve
	survivalWarnCutoff  = 0.10    // warn when P(success | alive) < 10%
	survivalTermCutoff  = 0.03    // terminate when P(success | alive) < 3%
	survivalMinSamples  = 20      // minimum observations before using learned values
	survivalMaxTimeSecs = 7200    // ignore bootstrap times beyond 2h (clock skew)
	survivalMinWarn     = 3 * 60  // floor: warn no earlier than 3m
	survivalMinTerm     = 5 * 60  // floor: terminate no earlier than 5m
	defaultWarnTimeout  = 15 * 60 // fallback when insufficient data
	defaultTermTimeout  = 20 * 60 // fallback when insufficient data
)

// bootstrapObservation represents a single instance's bootstrap outcome.
type bootstrapObservation struct {
	eventTime    int // seconds from launch to bootstrap (success) or death (failure)
	bootstrapped bool
}

// ComputeBootstrapSurvival queries historical launch data and returns learned
// warn/terminate thresholds for the given provider using survival analysis.
//
// The survival function answers: "given that T seconds have elapsed with no
// bootstrap progress, what fraction of instances in this situation eventually
// bootstrapped?" When that fraction drops below the warn/terminate cutoffs,
// we should act.
func ComputeBootstrapSurvival(database *sql.DB, provider string) (*BootstrapSurvival, error) {
	obs, err := queryBootstrapObservations(database, provider)
	if err != nil {
		return nil, err
	}

	result := &BootstrapSurvival{
		Provider:       provider,
		SampleSize:     len(obs),
		WarnAfter:      time.Duration(defaultWarnTimeout) * time.Second,
		TerminateAfter: time.Duration(defaultTermTimeout) * time.Second,
	}

	if len(obs) < survivalMinSamples {
		return result, nil
	}

	warnSecs, termSecs := computeSurvivalThresholds(obs)
	if warnSecs > 0 {
		result.WarnAfter = time.Duration(warnSecs) * time.Second
	}
	if termSecs > 0 {
		result.TerminateAfter = time.Duration(termSecs) * time.Second
	}

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
func queryBootstrapObservations(database *sql.DB, provider string) ([]bootstrapObservation, error) {
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
			CASE WHEN fa.wrapper_start IS NOT NULL THEN 1 ELSE 0 END AS bootstrapped
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

	rows, err := database.Query(query, provider, survivalMaxTimeSecs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var obs []bootstrapObservation
	for rows.Next() {
		var o bootstrapObservation
		if err := rows.Scan(&o.eventTime, &o.bootstrapped); err != nil {
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
func computeSurvivalThresholds(obs []bootstrapObservation) (warnSecs, termSecs int) {
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
		if o.bootstrapped {
			willBootstrap++
		}
	}

	maxTime := obs[len(obs)-1].eventTime
	idx := 0 // next observation to subtract

	for t := 0; t <= maxTime; t += survivalBucketSecs {
		// Subtract observations whose eventTime <= t (no longer at risk).
		for idx < len(obs) && obs[idx].eventTime <= t {
			atRisk--
			if obs[idx].bootstrapped {
				willBootstrap--
			}
			idx++
		}

		if atRisk == 0 {
			break
		}

		p := float64(willBootstrap) / float64(atRisk)

		if warnSecs == 0 && p < survivalWarnCutoff && t >= survivalMinWarn {
			warnSecs = t
		}
		if termSecs == 0 && p < survivalTermCutoff && t >= survivalMinTerm {
			termSecs = t
		}

		if warnSecs > 0 && termSecs > 0 {
			break
		}
	}

	return warnSecs, termSecs
}
