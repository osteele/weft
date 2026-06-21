package cmd

import (
	"context"
	"database/sql"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/syncorch"
)

type cloudSyncResult struct {
	Updated         int
	ReconcileResult *campaign.ReconcileResult
	Warnings        []string
}

const (
	// FastCloudSyncTimeout bounds startup cloud reconciliation for read-oriented commands.
	FastCloudSyncTimeout = syncorch.FastCloudTimeoutCLI
	// NormalCloudSyncTimeout is used for background/full cloud reconciliation passes.
	NormalCloudSyncTimeout = syncorch.NormalCloudTimeoutCLI
)

func syncCloudState(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, verbose bool) cloudSyncResult {
	res := syncorch.SyncCloud(cfg, database, syncorch.CloudSyncOptions{
		Verbose:     verbose,
		Reconciler:  reconciler,
		SyncResults: true,
		Timeout:     0,
	})
	return cloudSyncResult{Updated: res.Updated, ReconcileResult: res.ReconcileResult, Warnings: res.Warnings}
}

// syncCloudStateWithTimeout runs a cloud sync in a goroutine and returns false
// if it does not complete within timeout. The sync may continue in the
// background and still update the database later.
func syncCloudStateWithTimeout(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, timeout time.Duration, verbose bool) (cloudSyncResult, bool) {
	return syncCloudStateWithTimeoutAndResults(context.Background(), cfg, database, reconciler, timeout, verbose, true)
}

func syncCloudStateWithTimeoutAndResults(ctx context.Context, cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, timeout time.Duration, verbose bool, syncResults bool) (cloudSyncResult, bool) {
	res := syncorch.SyncCloud(cfg, database, syncorch.CloudSyncOptions{
		Context:     ctx,
		Verbose:     verbose,
		Reconciler:  reconciler,
		SyncResults: syncResults,
		Timeout:     timeout,
	})
	return cloudSyncResult{Updated: res.Updated, ReconcileResult: res.ReconcileResult, Warnings: res.Warnings}, res.Completed
}

func syncCloudStateWithClients(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, clients []cloud.Client, r2Client *r2.Client, verbose bool) cloudSyncResult {
	clientAny := make([]any, 0, len(clients))
	for _, client := range clients {
		clientAny = append(clientAny, client)
	}
	res := syncorch.SyncCloud(cfg, database, syncorch.CloudSyncOptions{
		Verbose:     verbose,
		Reconciler:  reconciler,
		Clients:     clientAny,
		R2Client:    r2Client,
		SyncResults: true,
		Timeout:     0,
	})
	return cloudSyncResult{Updated: res.Updated, ReconcileResult: res.ReconcileResult, Warnings: res.Warnings}
}

// syncCloudStateWithClientsTimeout runs a cloud sync with pre-built clients in
// a goroutine and returns false if it does not complete within timeout. The
// sync may continue in the background and still update the database later.
func syncCloudStateWithClientsTimeout(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, clients []cloud.Client, r2Client *r2.Client, timeout time.Duration, verbose bool) (cloudSyncResult, bool) {
	clientAny := make([]any, 0, len(clients))
	for _, client := range clients {
		clientAny = append(clientAny, client)
	}
	res := syncorch.SyncCloud(cfg, database, syncorch.CloudSyncOptions{
		Verbose:     verbose,
		Reconciler:  reconciler,
		Clients:     clientAny,
		R2Client:    r2Client,
		SyncResults: true,
		Timeout:     timeout,
	})
	return cloudSyncResult{Updated: res.Updated, ReconcileResult: res.ReconcileResult, Warnings: res.Warnings}, res.Completed
}
