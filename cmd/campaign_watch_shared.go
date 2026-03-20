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
// launches new instances for orphaned cloud jobs. Returns the new instance IDs.
func attemptRelaunchOrphanedJobs(database *sql.DB, cfg *config.Config) ([]int64, error) {
	// Reset jobs on terminal instances so they become unplaced
	if _, err := db.ResetJobsOnTerminalCloudInstances(database); err != nil {
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
	relaunchCfg := campaign.RelaunchConfig{
		Clients:    clients,
		R2Cfg:      r2Cfg,
		LaunchOpts: campaign.LaunchOpts{GracePeriodSeconds: 15 * 60},
		Database:   database,
	}

	result, err := campaign.RelaunchOrphanedJobs(relaunchCfg)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.InstanceIDs, nil
}
