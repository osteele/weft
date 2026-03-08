package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

// ReconcileCloudInstances checks all running/launching instances against the
// cloud provider and marks dead ones as failed (or completed if R2 has
// a completion marker). Returns the number of instances reconciled.
// r2Client may be nil, in which case completion detection is skipped.
func ReconcileCloudInstances(database *sql.DB, clients []cloud.Client, r2Client *r2.Client) (int, error) {
	instances, err := db.ListRunningCloudInstances(database)
	if err != nil {
		return 0, err
	}

	reconciled := 0
	for _, ci := range instances {
		providerID := ci.EffectiveProviderID()
		if providerID == "" {
			continue
		}

		client := clientForProvider(clients, cloud.Provider(ci.Provider))
		if client == nil {
			continue
		}

		inst, err := client.ShowInstance(providerID)
		if err != nil && !errors.Is(err, cloud.ErrInstanceNotFound) {
			log.Printf("reconcile: ShowInstance(%s) for instance %d: %v", providerID, ci.ID, err)
			continue
		}

		// If provider reports terminal state (or instance is gone) but DB doesn't, reconcile
		if isProviderTerminal(inst) && !IsInstanceTerminal(ci.Status) {
			status := "not found"
			if inst != nil {
				status = inst.Status
			}

			// Check R2 for completion marker before assuming failure.
			if hasR2CompletionMarker(r2Client, ci.ID) {
				log.Printf("reconcile: instance %d completed (R2 marker found), marking completed", ci.ID)
				if err := db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusCompleted); err != nil {
					log.Printf("reconcile: update instance %d status: %v", ci.ID, err)
				}
				reconciled++
				continue
			}

			log.Printf("reconcile: instance %d (provider %s) is dead (provider status: %s), marking failed", ci.ID, providerID, status)

			if err := db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusFailed); err != nil {
				log.Printf("reconcile: update instance %d status: %v", ci.ID, err)
				continue
			}
			if resetCount, err := db.ResetCloudInstanceJobs(database, ci.ID, db.AttemptOutcomeOrphaned); err != nil {
				log.Printf("reconcile: reset jobs for instance %d: %v", ci.ID, err)
			} else if resetCount > 0 {
				log.Printf("reconcile: reset %d jobs from instance %d to needs_rental", resetCount, ci.ID)
			}
			reconciled++
		}
	}

	return reconciled, nil
}

// ReconcileCampaigns checks active campaigns and marks them as completed or failed
// when all their instances have reached a terminal state.
func ReconcileCampaigns(database *sql.DB) error {
	campaigns, err := db.ListActiveCampaigns(database)
	if err != nil {
		return fmt.Errorf("list active campaigns: %w", err)
	}

	for _, c := range campaigns {
		instances, err := db.GetCampaignInstances(database, c.ID)
		if err != nil {
			log.Printf("reconcile campaigns: get instances for campaign %d: %v", c.ID, err)
			continue
		}
		if len(instances) == 0 {
			continue
		}

		allTerminal := true
		for _, inst := range instances {
			if !IsInstanceTerminal(inst.Status) {
				allTerminal = false
				break
			}
		}
		if !allTerminal {
			continue
		}

		// All instances are terminal; check if all failed
		allFailed := true
		for _, inst := range instances {
			if inst.Status != db.CloudInstanceStatusFailed {
				allFailed = false
				break
			}
		}

		status := db.CampaignStatusCompleted
		if allFailed {
			status = db.CampaignStatusFailed
		}
		if err := db.UpdateCampaignStatus(database, c.ID, status); err != nil {
			log.Printf("reconcile campaigns: update campaign %d to %s: %v", c.ID, status, err)
		} else {
			log.Printf("reconcile campaigns: campaign %d → %s", c.ID, status)
		}
	}
	return nil
}

// hasR2CompletionMarker checks whether the wrapper wrote a completion marker
// to R2 before the instance self-destructed. Returns false if r2Client is nil.
func hasR2CompletionMarker(r2Client *r2.Client, instanceID int64) bool {
	if r2Client == nil {
		return false
	}
	key := fmt.Sprintf("campaigns/%d/.complete", instanceID)
	exists, err := r2Client.ObjectExists(context.Background(), key)
	return err == nil && exists
}

// isProviderTerminal returns true if the provider instance is in a terminal/dead state.
func isProviderTerminal(inst *cloud.Instance) bool {
	if inst == nil {
		return true // instance not found = dead
	}
	switch inst.Status {
	case "exited", "destroyed", "error", "dead", "stopped":
		return true
	default:
		return false
	}
}
