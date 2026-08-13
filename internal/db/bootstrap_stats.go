package db

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// BootstrapDurations is a sorted (ascending) slice of successful bootstrap
// durations from historical instances. It supports conditional median queries
// for estimating remaining time given that a bootstrap has already taken some
// known elapsed time.
type BootstrapDurations []time.Duration

const bootstrapStageStatsMinSpan = 14 * 24 * time.Hour

// BootstrapStageStatsMinSpan is the required age of per-stage transition
// history before UI ETA estimates prefer per-stage samples over total
// bootstrap samples.
func BootstrapStageStatsMinSpan() time.Duration {
	return bootstrapStageStatsMinSpan
}

// initBootstrapTransitionsSchema creates the append-only stage transition log.
func initBootstrapTransitionsSchema(database *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS bootstrap_transitions (
			launch_id INTEGER NOT NULL REFERENCES launches(id),
			stage TEXT NOT NULL,
			entered_at INTEGER NOT NULL,
			PRIMARY KEY (launch_id, stage, entered_at)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bootstrap_transitions_stage_entered
			ON bootstrap_transitions(stage, entered_at)`,
		`CREATE INDEX IF NOT EXISTS idx_bootstrap_transitions_launch_entered
			ON bootstrap_transitions(launch_id, entered_at)`,
	}
	for _, stmt := range stmts {
		if _, err := database.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// TotalBootstrapPercentile returns a percentile over successful total bootstrap
// durations, measured from launch creation to the first wrapper start.
func TotalBootstrapPercentile(database *sql.DB, p float64) (time.Duration, int, error) {
	if p < 0 || p > 1 {
		return 0, 0, fmt.Errorf("percentile must be between 0 and 1")
	}
	durations, err := TotalBootstrapDurations(database)
	if err != nil {
		return 0, 0, err
	}
	if len(durations) == 0 {
		return 0, 0, nil
	}
	return bootstrapDurationPercentile(durations, p), len(durations), nil
}

// TotalBootstrapDurations returns sorted successful total bootstrap durations,
// measured from launch creation to the first wrapper start.
func TotalBootstrapDurations(database *sql.DB) (BootstrapDurations, error) {
	if database == nil {
		return nil, nil
	}
	rows, err := database.Query(`
		WITH first_attempt AS (
			SELECT ja.launch_id, MIN(jpt.wrapper_start) AS wrapper_start
			FROM job_attempts ja
			JOIN job_phase_timings jpt ON jpt.job_id = ja.job_id
			WHERE ja.launch_id IS NOT NULL
			  AND jpt.wrapper_start IS NOT NULL
			GROUP BY ja.launch_id
		)
		SELECT fa.wrapper_start - l.created_at AS duration_secs
		FROM launches l
		JOIN first_attempt fa ON fa.launch_id = l.id
		WHERE l.created_at > 0
		  AND fa.wrapper_start > l.created_at
		  AND (fa.wrapper_start - l.created_at) BETWEEN 1 AND ?
	`, bootstrapSurvivalConfig.MaxTimeSecs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var durations BootstrapDurations
	for rows.Next() {
		var secs int
		if err := rows.Scan(&secs); err != nil {
			return nil, err
		}
		durations = append(durations, time.Duration(secs)*time.Second)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return durations, nil
}

// BootstrapStagePercentile returns a percentile over completed durations spent
// in a bootstrap stage, plus sample count and the oldest sample timestamp.
func BootstrapStagePercentile(database *sql.DB, stage string, p float64) (time.Duration, int, int64, error) {
	if p < 0 || p > 1 {
		return 0, 0, 0, fmt.Errorf("percentile must be between 0 and 1")
	}
	durations, oldest, err := BootstrapStageDurations(database, stage)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(durations) == 0 {
		return 0, 0, 0, nil
	}
	return bootstrapDurationPercentile(durations, p), len(durations), oldest, nil
}

// BootstrapStageDurations returns sorted completed durations spent in a
// bootstrap stage, plus the oldest stage-entry timestamp in the sample.
func BootstrapStageDurations(database *sql.DB, stage string) (BootstrapDurations, int64, error) {
	if database == nil {
		return nil, 0, nil
	}
	rows, err := database.Query(`
		WITH ordered AS (
			SELECT
				bt.launch_id,
				bt.stage,
				bt.entered_at,
				LEAD(bt.entered_at) OVER (
					PARTITION BY bt.launch_id
					ORDER BY bt.entered_at
				) AS next_entered_at
			FROM bootstrap_transitions bt
		)
		SELECT entered_at, next_entered_at - entered_at AS duration_secs
		FROM ordered
		WHERE stage = ?
		  AND next_entered_at IS NOT NULL
		  AND next_entered_at > entered_at
		  AND (next_entered_at - entered_at) BETWEEN 1 AND ?
	`, stage, bootstrapSurvivalConfig.MaxTimeSecs)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var durations BootstrapDurations
	oldest := int64(0)
	for rows.Next() {
		var enteredAt int64
		var secs int
		if err := rows.Scan(&enteredAt, &secs); err != nil {
			return nil, 0, err
		}
		if oldest == 0 || enteredAt < oldest {
			oldest = enteredAt
		}
		durations = append(durations, time.Duration(secs)*time.Second)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return durations, oldest, nil
}

func bootstrapDurationPercentile(durations BootstrapDurations, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	idx := int(p * float64(len(durations)-1))
	return durations[idx]
}

// LatestBootstrapStageEnteredAt returns the most recent time a launch entered
// the specified stage.
func LatestBootstrapStageEnteredAt(database *sql.DB, launchID int64, stage string) (int64, error) {
	if database == nil || launchID <= 0 || stage == "" {
		return 0, nil
	}
	var enteredAt int64
	err := database.QueryRow(`
		SELECT entered_at
		FROM bootstrap_transitions
		WHERE launch_id = ? AND stage = ?
		ORDER BY entered_at DESC
		LIMIT 1
	`, launchID, stage).Scan(&enteredAt)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return enteredAt, nil
}

// HasBootstrapTransitions reports whether any bootstrap stage has been recorded
// for a launch.
func HasBootstrapTransitions(database *sql.DB, launchID int64) (bool, error) {
	if database == nil || launchID <= 0 {
		return false, nil
	}
	var exists int
	err := database.QueryRow(`
		SELECT EXISTS(
			SELECT 1
			FROM bootstrap_transitions
			WHERE launch_id = ?
		)
	`, launchID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists != 0, nil
}

// Percentile returns a duration percentile from a sorted bootstrap duration
// sample.
func (d BootstrapDurations) Percentile(p float64) (time.Duration, bool) {
	if len(d) == 0 || p < 0 || p > 1 {
		return 0, false
	}
	return bootstrapDurationPercentile(d, p), true
}

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

// BootstrapSurvival holds bootstrap timeout thresholds derived from historical
// instance data using survival analysis, plus whether each threshold was
// learned or retained its configured default. At each time T, it
// computes the fraction of instances that were alive at T (without having
// succeeded) that eventually did bootstrap. The warn and terminate
// thresholds are the times at which this probability drops below configured
// cutoffs.
type BootstrapSurvival struct {
	Provider         string
	SampleSize       int
	WarnAfter        time.Duration      // time at which P(success) < warnCutoff
	WarnLearned      bool               // true when WarnAfter came from the observed survival curve
	TerminateAfter   time.Duration      // time at which P(success) < terminateCutoff
	TerminateLearned bool               // true when TerminateAfter came from the observed survival curve
	Durations        BootstrapDurations // sorted successful bootstrap durations for conditional estimates
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
		result.WarnLearned = true
	}
	if termSecs > 0 {
		result.TerminateAfter = time.Duration(termSecs) * time.Second
		result.TerminateLearned = true
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
// Success: the instance eventually started a job (any attempt's wrapper_start is set).
//
//	event_time = wrapper_start - COALESCE(provider_running_at, launched_at)
//
// Failure: the instance died without starting a job.
//
//	event_time = ended_at - COALESCE(provider_running_at, launched_at)
func queryBootstrapObservations(database *sql.DB, provider string) ([]survivalObservation, error) {
	query := `
		WITH first_attempt AS (
			SELECT ja.launch_id, MIN(jpt.wrapper_start) AS wrapper_start
			FROM job_attempts ja
			JOIN job_phase_timings jpt ON jpt.job_id = ja.job_id
			WHERE ja.launch_id IS NOT NULL
			  AND jpt.wrapper_start IS NOT NULL
			GROUP BY ja.launch_id
		)
		SELECT
			CASE
				WHEN fa.wrapper_start IS NOT NULL
				THEN fa.wrapper_start - COALESCE(l.provider_running_at, l.launched_at)
				ELSE l.ended_at - COALESCE(l.provider_running_at, l.launched_at)
			END AS event_time,
			CASE WHEN fa.wrapper_start IS NOT NULL THEN 1 ELSE 0 END AS succeeded
		FROM launches l
		LEFT JOIN first_attempt fa ON fa.launch_id = l.id
		WHERE l.launched_at IS NOT NULL
		  AND l.status IN ('completed', 'failed')
		  AND l.provider = ?
		  AND CASE
			WHEN fa.wrapper_start IS NOT NULL
			THEN (fa.wrapper_start - COALESCE(l.provider_running_at, l.launched_at)) BETWEEN 1 AND ?
			ELSE (l.ended_at - COALESCE(l.provider_running_at, l.launched_at)) > 0
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
