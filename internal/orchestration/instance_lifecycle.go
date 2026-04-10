package orchestration

import (
	"database/sql"
	"fmt"
	"sync"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/runpod"
	"github.com/osteele/weft/internal/vastai"
)

// TerminateInstancesParallel terminates multiple cloud instances in parallel.
func TerminateInstancesParallel(database *sql.DB, instanceIDs []int64) (int, []error) {
	var mu sync.Mutex
	var terminated int
	var errors []error
	var wg sync.WaitGroup

	for _, id := range instanceIDs {
		wg.Add(1)
		go func(instanceID int64) {
			defer wg.Done()

			ci, err := db.GetLaunch(database, instanceID)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("get instance %s: %w", ids.FormatInstanceID(instanceID), err))
				mu.Unlock()
				return
			}
			if ci == nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("instance %s not found", ids.FormatInstanceID(instanceID)))
				mu.Unlock()
				return
			}
			if campaign.IsInstanceTerminal(ci.Status) {
				return
			}

			providerInstID := ci.EffectiveProviderID()
			if providerInstID != "" {
				client := cloudClientForDBInstance(ci.Provider)
				if err := client.DestroyInstance(providerInstID); err != nil {
					mu.Lock()
					errors = append(errors, fmt.Errorf("destroy %s instance %s: %w", ci.Provider, providerInstID, err))
					mu.Unlock()
				}
			}

			if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusCancelled, db.TerminationReasonCancelled); err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("update instance %s status: %w", ids.FormatInstanceID(instanceID), err))
				mu.Unlock()
				return
			}
			if _, err := db.ResetLaunchJobs(database, instanceID, db.AttemptOutcomeCancelled); err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("reset jobs for instance %s: %w", ids.FormatInstanceID(instanceID), err))
				mu.Unlock()
				return
			}
			mu.Lock()
			terminated++
			mu.Unlock()
		}(id)
	}

	wg.Wait()
	return terminated, errors
}

func cloudClientForDBInstance(provider string) cloud.Client {
	switch cloud.Provider(provider) {
	case cloud.ProviderVastai:
		return vastai.NewCloudClient(vastai.NewClient())
	case cloud.ProviderRunpod:
		return runpod.NewCloudClient()
	default:
		return vastai.NewCloudClient(vastai.NewClient())
	}
}
