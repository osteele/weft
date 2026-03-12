package cloudsync

import (
	"database/sql"
	"log"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/r2"
)

type Result struct {
	Updated         int
	ReconcileResult *campaign.ReconcileResult
}

func SyncState(database *sql.DB, reconciler *campaign.Reconciler, clients []cloud.Client, r2Client *r2.Client, syncResults func() int) Result {
	if reconciler == nil {
		reconciler = campaign.NewReconciler()
	}

	result := Result{}
	if len(clients) > 0 {
		reconcileResult, err := reconciler.ReconcileCloudInstances(database, clients, r2Client)
		if err != nil {
			log.Printf("reconcile: %v", err)
		} else {
			result.ReconcileResult = reconcileResult
			if reconcileResult != nil {
				result.Updated += reconcileResult.Reconciled
			}
		}
	}

	if syncResults != nil {
		result.Updated += syncResults()
	}

	if _, err := campaign.ReconcileCampaigns(database); err != nil {
		log.Printf("reconcile campaigns: %v", err)
	}

	return result
}
