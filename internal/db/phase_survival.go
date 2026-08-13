package db

import (
	"database/sql"
	"time"
)

// SetupSurvival holds setup-phase timeout thresholds derived from historical
// job data using survival analysis, plus whether each threshold was learned
// or retained its configured default. At each time T, it computes
// the fraction of jobs whose setup was still running at T that eventually
// completed setup. The warn and terminate thresholds are the times at which
// this probability drops below configured cutoffs.
type SetupSurvival struct {
	SampleSize       int
	WarnAfter        time.Duration
	WarnLearned      bool
	TerminateAfter   time.Duration
	TerminateLearned bool
}

// setupSurvivalConfig is the configuration for setup-phase stall detection.
var setupSurvivalConfig = survivalConfig{
	BucketSecs:     30,
	WarnCutoff:     0.10,
	TermCutoff:     0.03,
	MinSamples:     20,
	MaxTimeSecs:    3600, // ignore setup times beyond 1h
	MinWarnSecs:    5 * 60,
	MinTermSecs:    10 * 60,
	DefaultWarnSec: 15 * 60,
	DefaultTermSec: 25 * 60,
}

// ComputeSetupSurvival queries historical setup phase durations and returns
// learned warn/terminate thresholds using survival analysis.
//
// It uses a tiered scoping strategy:
//  1. Filter by command AND working_dir — if >= 20 samples, use those
//  2. Filter by working_dir only — if >= 20, use those
//  3. All jobs with setup timing data
func ComputeSetupSurvival(database *sql.DB, command, workingDir string) (*SetupSurvival, error) {
	cfg := setupSurvivalConfig

	// Tier 1: command + workspace
	obs, err := querySetupObservations(database, command, workingDir)
	if err != nil {
		return nil, err
	}

	// Tier 2: workspace only
	if len(obs) < cfg.MinSamples && workingDir != "" {
		obs, err = querySetupObservations(database, "", workingDir)
		if err != nil {
			return nil, err
		}
	}

	// Tier 3: all jobs
	if len(obs) < cfg.MinSamples {
		obs, err = querySetupObservations(database, "", "")
		if err != nil {
			return nil, err
		}
	}

	result := &SetupSurvival{
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

	return result, nil
}

// querySetupObservations fetches setup-phase observations for survival analysis.
//
// Success: setup completed (setup_end is set).
//
//	event_time = setup_end - setup_start
//
// Failure: setup started but never completed (job failed or instance died).
//
//	event_time = COALESCE(end_time, ended_at) - setup_start
//
// Filters are applied when non-empty: command matches the job's command,
// workingDir matches the job's working_dir.
func querySetupObservations(database *sql.DB, command, workingDir string) ([]survivalObservation, error) {
	query := `
		SELECT
			CASE
				WHEN jpt.setup_end IS NOT NULL
				THEN jpt.setup_end - jpt.setup_start
				ELSE COALESCE(ja.end_time, l.ended_at, jpt.setup_start + ?) - jpt.setup_start
			END AS event_time,
			CASE WHEN jpt.setup_end IS NOT NULL THEN 1 ELSE 0 END AS succeeded
		FROM job_phase_timings jpt
		JOIN jobs j ON j.id = jpt.job_id AND j.tombstoned = 0
		JOIN job_attempts ja ON ja.job_id = j.id AND ja.attempt_number = 1
		LEFT JOIN launches l ON l.id = ja.launch_id
		WHERE jpt.setup_start IS NOT NULL
		  AND (? = '' OR j.command = ?)
		  AND (? = '' OR j.working_dir = ?)
		  AND CASE
			WHEN jpt.setup_end IS NOT NULL
			THEN (jpt.setup_end - jpt.setup_start) BETWEEN 1 AND ?
			ELSE COALESCE(ja.end_time, l.ended_at, jpt.setup_start + ?) - jpt.setup_start > 0
		  END
	`

	cfg := setupSurvivalConfig
	rows, err := database.Query(query,
		cfg.MaxTimeSecs,  // fallback for missing end times
		command, command, // command filter
		workingDir, workingDir, // workingDir filter
		cfg.MaxTimeSecs, // max setup time for successes
		cfg.MaxTimeSecs, // fallback for failure end time
	)
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
