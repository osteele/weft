package cloudsync

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/r2"
)

type Result struct {
	Updated         int
	ReconcileResult *campaign.ReconcileResult
}

func SyncState(ctx context.Context, database *sql.DB, reconciler *campaign.Reconciler, clients []cloud.Client, r2Client *r2.Client, syncResults func(context.Context) int) Result {
	if ctx == nil {
		ctx = context.Background()
	}
	if reconciler == nil {
		reconciler = campaign.NewReconciler()
	}

	result := Result{}

	// ReconcileLaunches and syncResults touch disjoint R2 prefixes and DB
	// columns, so they run concurrently. ReconcileCampaigns reads instance
	// state produced by ReconcileLaunches, so it runs after the join.
	var (
		wg              sync.WaitGroup
		reconcileResult *campaign.ReconcileResult
		resultsUpdated  int
	)
	if len(clients) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := reconciler.ReconcileLaunches(ctx, database, clients, r2Client)
			if err != nil {
				slog.Warn("reconcile failed", "component", "cloudsync", "error", err)
				return
			}
			reconcileResult = r
		}()
	}
	if syncResults != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resultsUpdated = syncResults(ctx)
		}()
	}
	wg.Wait()

	result.ReconcileResult = reconcileResult
	if reconcileResult != nil {
		result.Updated += reconcileResult.Reconciled + reconcileResult.JobsUpdated
	}
	result.Updated += resultsUpdated

	if _, err := campaign.ReconcileCampaigns(database); err != nil {
		slog.Warn("reconcile campaigns failed", "component", "cloudsync", "error", err)
	}

	return result
}
