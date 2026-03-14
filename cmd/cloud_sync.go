package cmd

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudsync"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/r2"
)

type cloudSyncResult struct {
	Updated         int
	ReconcileResult *campaign.ReconcileResult
}

func syncCloudState(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, verbose bool) cloudSyncResult {
	if cfg == nil {
		cfg = &config.Config{}
	}
	r2Client, err := buildR2Client(cfg)
	if err != nil && verbose {
		fmt.Printf("Warning: R2 client: %v\n", err)
	}
	clients, _ := buildCloudClients(cfg)
	return syncCloudStateWithClients(cfg, database, reconciler, clients, r2Client, verbose)
}

func syncCloudStateWithClients(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, clients []cloud.Client, r2Client *r2.Client, verbose bool) cloudSyncResult {
	if cfg == nil {
		cfg = &config.Config{}
	}
	result := cloudsync.SyncState(database, reconciler, clients, r2Client, func() int {
		return syncCloudJobResults(cfg, database, verbose)
	})
	if verbose && result.ReconcileResult != nil && result.ReconcileResult.Reconciled > 0 {
		fmt.Printf("Reconciled %d cloud instance(s)\n", result.ReconcileResult.Reconciled)
	}
	return cloudSyncResult(result)
}
