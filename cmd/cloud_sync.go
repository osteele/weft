package cmd

import (
	"database/sql"
	"fmt"
	"time"

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

const (
	// FastCloudSyncTimeout bounds startup cloud reconciliation for read-oriented commands.
	FastCloudSyncTimeout = 5 * time.Second
	// NormalCloudSyncTimeout is used for background/full cloud reconciliation passes.
	NormalCloudSyncTimeout = 30 * time.Second
)

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

// syncCloudStateWithTimeout runs a cloud sync in a goroutine and returns false
// if it does not complete within timeout. The sync may continue in the
// background and still update the database later.
func syncCloudStateWithTimeout(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, timeout time.Duration, verbose bool) (cloudSyncResult, bool) {
	if timeout <= 0 {
		return syncCloudState(cfg, database, reconciler, verbose), true
	}

	done := make(chan cloudSyncResult, 1)
	go func() {
		done <- syncCloudState(cfg, database, reconciler, verbose)
	}()

	select {
	case result := <-done:
		return result, true
	case <-time.After(timeout):
		return cloudSyncResult{}, false
	}
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

// syncCloudStateWithClientsTimeout runs a cloud sync with pre-built clients in
// a goroutine and returns false if it does not complete within timeout. The
// sync may continue in the background and still update the database later.
func syncCloudStateWithClientsTimeout(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, clients []cloud.Client, r2Client *r2.Client, timeout time.Duration, verbose bool) (cloudSyncResult, bool) {
	if timeout <= 0 {
		return syncCloudStateWithClients(cfg, database, reconciler, clients, r2Client, verbose), true
	}

	done := make(chan cloudSyncResult, 1)
	go func() {
		done <- syncCloudStateWithClients(cfg, database, reconciler, clients, r2Client, verbose)
	}()

	select {
	case result := <-done:
		return result, true
	case <-time.After(timeout):
		return cloudSyncResult{}, false
	}
}
