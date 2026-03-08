package campaign

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// ReconcileCloudInstances checks all running/launching instances against the
// cloud provider and marks dead ones as failed. Returns the number of instances reconciled.
func ReconcileCloudInstances(database *sql.DB, clients []cloud.Client) (int, error) {
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
