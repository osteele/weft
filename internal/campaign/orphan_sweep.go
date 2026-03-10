package campaign

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// weftLabelPrefix is the label prefix used for Vast.ai instances.
const weftLabelPrefix = "weft/"

// weftNamePrefix is the name prefix used for RunPod instances.
const weftNamePrefix = "weft-"

// orphanSweepInterval is the minimum time between orphan sweeps.
const orphanSweepInterval = 5 * time.Minute

var (
	lastOrphanSweepMu sync.Mutex
	lastOrphanSweep   time.Time
)

// SweepOrphanedInstances lists all instances from each provider, filters to
// weft-labeled ones, cross-references with the local DB, and destroys any that
// belong to non-active campaigns or have no matching DB record.
// Only instances with the weft label/name prefix are considered — unlabeled
// instances are never touched.
func SweepOrphanedInstances(database *sql.DB, clients []cloud.Client) (destroyed int, err error) {
	for _, client := range clients {
		instances, listErr := client.ListAllInstances()
		if listErr != nil {
			log.Printf("orphan-sweep: %s ListAllInstances: %v", client.Provider(), listErr)
			continue
		}

		for _, inst := range instances {
			campaignID, ok := extractCampaignID(inst.Label)
			if !ok {
				continue
			}

			if isProviderTerminal(&inst) {
				continue
			}

			shouldDestroy, reason := shouldDestroyOrphan(database, campaignID, inst.ProviderID)
			if !shouldDestroy {
				continue
			}

			log.Printf("orphan-sweep: destroying %s instance %s (label=%q, reason=%s)",
				client.Provider(), inst.ProviderID, inst.Label, reason)
			if destroyErr := client.DestroyInstance(inst.ProviderID); destroyErr != nil {
				log.Printf("orphan-sweep: failed to destroy %s: %v", inst.ProviderID, destroyErr)
				continue
			}
			destroyed++
		}
	}
	return destroyed, nil
}

// shouldDestroyOrphan checks whether a weft-labeled instance should be destroyed.
// Returns (true, reason) if the instance is orphaned.
func shouldDestroyOrphan(database *sql.DB, campaignID int64, providerID string) (bool, string) {
	// Check if the campaign exists
	campaign, err := db.GetCampaign(database, campaignID)
	if err != nil {
		log.Printf("orphan-sweep: get campaign %d: %v", campaignID, err)
		return false, ""
	}
	if campaign == nil {
		return true, "campaign not found in DB"
	}

	// Check if the campaign is active (non-terminal)
	switch campaign.Status {
	case db.CampaignStatusPlanned, db.CampaignStatusLaunching, db.CampaignStatusRunning:
		// Campaign is active — check if this provider ID is tracked in the DB
		instances, err := db.GetCampaignInstances(database, campaignID)
		if err != nil {
			log.Printf("orphan-sweep: get campaign %d instances: %v", campaignID, err)
			return false, ""
		}
		for _, ci := range instances {
			if ci.EffectiveProviderID() == providerID {
				return false, "" // tracked instance, leave it alone
			}
		}
		return true, fmt.Sprintf("provider ID %s not tracked in active campaign %d", providerID, campaignID)
	default:
		// Campaign is terminal (completed/failed/cancelled) — instance is orphaned
		return true, fmt.Sprintf("campaign %d is %s", campaignID, campaign.Status)
	}
}

// extractCampaignID parses a campaign ID from a weft label/name.
// Accepts "weft/c42" (Vast.ai label) or "weft-c42" (RunPod name).
func extractCampaignID(label string) (int64, bool) {
	var idStr string
	if strings.HasPrefix(label, weftLabelPrefix+"c") {
		idStr = label[len(weftLabelPrefix+"c"):]
	} else if strings.HasPrefix(label, weftNamePrefix+"c") {
		idStr = label[len(weftNamePrefix+"c"):]
	} else {
		return 0, false
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// MaybeSweepOrphanedInstances runs SweepOrphanedInstances if at least
// orphanSweepInterval has elapsed since the last sweep.
func MaybeSweepOrphanedInstances(database *sql.DB, clients []cloud.Client) (int, error) {
	lastOrphanSweepMu.Lock()
	if time.Since(lastOrphanSweep) < orphanSweepInterval {
		lastOrphanSweepMu.Unlock()
		return 0, nil
	}
	lastOrphanSweep = time.Now()
	lastOrphanSweepMu.Unlock()

	return SweepOrphanedInstances(database, clients)
}
