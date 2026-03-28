package cmd

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

// attemptRelaunchOrphanedJobs resets jobs on terminal cloud instances and
// launches new instances for orphaned cloud jobs. extraAttempts raises the
// max attempt threshold (used for manual retries to allow more tries).
func attemptRelaunchOrphanedJobs(database *sql.DB, cfg *config.Config, extraAttempts int) (*campaign.RelaunchResult, error) {
	// Reset jobs on terminal instances so they become unplaced.
	// The map tells RelaunchOrphanedJobs which jobs were just orphaned
	// (and from which instance), so it doesn't sweep in unrelated unplaced jobs.
	resetJobs, err := db.ResetJobsOnTerminalLaunches(database)
	if err != nil {
		log.Printf("auto-relaunch: reset jobs: %v", err)
	}

	if cfg == nil {
		cfg, _ = config.Load()
	}
	clients, err := buildCloudClients(cfg)
	if err != nil || len(clients) == 0 {
		return nil, fmt.Errorf("no cloud providers available: %v", err)
	}

	r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
	survivalModel := buildSurvivalModel(database)
	relaunchCfg := campaign.RelaunchConfig{
		Clients:       clients,
		R2Cfg:         r2Cfg,
		LaunchOpts:    campaign.LaunchOpts{GracePeriodSeconds: 15 * 60},
		MaxAttempts:   campaign.DefaultMaxCloudAttempts + extraAttempts,
		SurvivalModel: survivalModel,
		MinSurvival:   campaignLaunchMinSurvival,
		Database:      database,
		ResetJobs:     resetJobs,
	}

	result, err := campaign.RelaunchOrphanedJobs(relaunchCfg)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &campaign.RelaunchResult{}, nil
	}
	return result, nil
}
