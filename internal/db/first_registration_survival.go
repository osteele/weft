package db

import (
	"database/sql"
	"fmt"
	"time"
)

const (
	FirstRegistrationScopeProviderDataCenter = "provider_data_center"
	FirstRegistrationScopeProvider           = "provider"
	FirstRegistrationScopeGlobal             = "global"
)

// FirstRegistrationScope identifies the preferred historical bucket for
// first-instance registration survival analysis.
type FirstRegistrationScope struct {
	Provider   string
	DataCenter string
}

// FirstRegistrationSurvival holds historical/default thresholds and conditional
// estimates for time-to-first-worker-registration from campaign creation.
type FirstRegistrationSurvival struct {
	ScopeLevel       string
	Provider         string
	DataCenter       string
	SampleSize       int
	WarnAfter        time.Duration
	WarnLearned      bool
	TerminateAfter   time.Duration
	TerminateLearned bool
	Durations        BootstrapDurations

	observations []survivalObservation
}

// ScopeDescription returns a compact human-readable scope label.
func (s *FirstRegistrationSurvival) ScopeDescription() string {
	if s == nil {
		return FirstRegistrationScopeGlobal
	}
	switch s.ScopeLevel {
	case FirstRegistrationScopeProviderDataCenter:
		return fmt.Sprintf("%s/%s", s.Provider, s.DataCenter)
	case FirstRegistrationScopeProvider:
		return s.Provider
	default:
		return FirstRegistrationScopeGlobal
	}
}

// ConditionalSuccess returns P(first registration eventually occurs | no
// registration yet at elapsed). Returns false when no historical at-risk tail
// exists at elapsed.
func (s *FirstRegistrationSurvival) ConditionalSuccess(elapsed time.Duration) (float64, bool) {
	if s == nil || len(s.observations) == 0 {
		return 0, false
	}
	secs := int(elapsed / time.Second)
	if secs < 0 {
		secs = 0
	}
	atRisk := 0
	willSucceed := 0
	for _, o := range s.observations {
		if o.eventTime <= secs {
			continue
		}
		atRisk++
		if o.succeeded {
			willSucceed++
		}
	}
	if atRisk == 0 {
		return 0, false
	}
	return float64(willSucceed) / float64(atRisk), true
}

type firstRegistrationSurvivalConfig struct {
	BucketSecs     int
	WarnCutoff     float64
	TermCutoff     float64
	MinSamples     int
	MaxTimeSecs    int
	MinWarnSecs    int
	MinTermSecs    int
	DefaultWarnSec int
	DefaultTermSec int
}

var firstRegistrationConfig = firstRegistrationSurvivalConfig{
	BucketSecs:     15,
	WarnCutoff:     0.20,
	TermCutoff:     0.05,
	MinSamples:     20,
	MaxTimeSecs:    3600,
	MinWarnSecs:    60,
	MinTermSecs:    180,
	DefaultWarnSec: 5 * 60,
	DefaultTermSec: 12 * 60,
}

// ComputeFirstRegistrationSurvival computes warn/terminate thresholds for
// "time from campaign creation to first worker launch registration". Each
// threshold records whether it came from the survival curve or its configured
// default.
//
// It attempts scopes in order:
//  1. provider + data center
//  2. provider
//  3. global
//
// The first scope with at least MinSamples is used; otherwise global is used.
func ComputeFirstRegistrationSurvival(database *sql.DB, scope FirstRegistrationScope) (*FirstRegistrationSurvival, error) {
	cfg := firstRegistrationConfig

	type candidate struct {
		level      string
		provider   string
		dataCenter string
	}
	candidates := make([]candidate, 0, 3)
	if scope.Provider != "" && scope.DataCenter != "" {
		candidates = append(candidates, candidate{
			level:      FirstRegistrationScopeProviderDataCenter,
			provider:   scope.Provider,
			dataCenter: scope.DataCenter,
		})
	}
	if scope.Provider != "" {
		candidates = append(candidates, candidate{
			level:    FirstRegistrationScopeProvider,
			provider: scope.Provider,
		})
	}
	candidates = append(candidates, candidate{level: FirstRegistrationScopeGlobal})

	var selected candidate
	var selectedObs []survivalObservation
	for i, cand := range candidates {
		obs, err := queryFirstRegistrationObservations(database, cand.level, cand.provider, cand.dataCenter)
		if err != nil {
			return nil, err
		}
		selected = cand
		selectedObs = obs
		last := i == len(candidates)-1
		if len(obs) >= cfg.MinSamples || last {
			break
		}
	}

	result := &FirstRegistrationSurvival{
		ScopeLevel:     selected.level,
		Provider:       selected.provider,
		DataCenter:     selected.dataCenter,
		SampleSize:     len(selectedObs),
		WarnAfter:      time.Duration(cfg.DefaultWarnSec) * time.Second,
		TerminateAfter: time.Duration(cfg.DefaultTermSec) * time.Second,
	}

	if len(selectedObs) < cfg.MinSamples {
		return result, nil
	}

	warnSecs, termSecs := computeSurvivalThresholds(selectedObs, survivalConfig{
		BucketSecs:     cfg.BucketSecs,
		WarnCutoff:     cfg.WarnCutoff,
		TermCutoff:     cfg.TermCutoff,
		MinSamples:     cfg.MinSamples,
		MaxTimeSecs:    cfg.MaxTimeSecs,
		MinWarnSecs:    cfg.MinWarnSecs,
		MinTermSecs:    cfg.MinTermSecs,
		DefaultWarnSec: cfg.DefaultWarnSec,
		DefaultTermSec: cfg.DefaultTermSec,
	})
	if warnSecs > 0 {
		result.WarnAfter = time.Duration(warnSecs) * time.Second
		result.WarnLearned = true
	}
	if termSecs > 0 {
		result.TerminateAfter = time.Duration(termSecs) * time.Second
		result.TerminateLearned = true
	}

	durations := make(BootstrapDurations, 0, len(selectedObs))
	for _, o := range selectedObs {
		if o.succeeded {
			durations = append(durations, time.Duration(o.eventTime)*time.Second)
		}
	}
	result.Durations = durations
	result.observations = append([]survivalObservation(nil), selectedObs...)
	return result, nil
}

func queryFirstRegistrationObservations(database *sql.DB, scopeLevel, provider, dataCenter string) ([]survivalObservation, error) {
	cfg := firstRegistrationConfig
	filter := ""
	args := []any{cfg.MaxTimeSecs, cfg.MaxTimeSecs}
	switch scopeLevel {
	case FirstRegistrationScopeProviderDataCenter:
		filter = `AND fw.provider = ? AND fw.data_center = ?`
		args = append(args, provider, dataCenter)
	case FirstRegistrationScopeProvider:
		filter = `AND fw.provider = ?`
		args = append(args, provider)
	case FirstRegistrationScopeGlobal:
		// no scope filter
	default:
		return nil, fmt.Errorf("invalid first registration scope level: %s", scopeLevel)
	}

	query := `
		WITH first_worker AS (
			SELECT
				l.campaign_id,
				l.created_at,
				l.provider,
				COALESCE(NULLIF(l.data_center, ''), '') AS data_center,
				ROW_NUMBER() OVER (PARTITION BY l.campaign_id ORDER BY l.created_at ASC, l.id ASC) AS rn
			FROM launches l
			WHERE l.campaign_id IS NOT NULL
			  AND COALESCE(l.instance_role, 'worker') = 'worker'
		)
		SELECT
			CASE
				WHEN fw.created_at IS NOT NULL
				THEN fw.created_at - c.created_at
				ELSE c.ended_at - c.created_at
			END AS event_time,
			CASE WHEN fw.created_at IS NOT NULL THEN 1 ELSE 0 END AS succeeded
		FROM campaigns c
		LEFT JOIN first_worker fw ON fw.campaign_id = c.id AND fw.rn = 1
		WHERE c.created_at IS NOT NULL
		  AND (
			fw.created_at IS NOT NULL
			OR (c.status IN (?, ?, ?) AND c.ended_at IS NOT NULL)
		  )
		  AND CASE
			WHEN fw.created_at IS NOT NULL
			THEN (fw.created_at - c.created_at) BETWEEN 0 AND ?
			ELSE (c.ended_at - c.created_at) BETWEEN 1 AND ?
		  END
		  ` + filter

	args = append([]any{
		CampaignStatusCompleted, CampaignStatusFailed, CampaignStatusCancelled,
	}, args...)

	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	obs := make([]survivalObservation, 0)
	for rows.Next() {
		var o survivalObservation
		if err := rows.Scan(&o.eventTime, &o.succeeded); err != nil {
			return nil, err
		}
		obs = append(obs, o)
	}
	return obs, rows.Err()
}
