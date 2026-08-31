package orchestration

import (
	"database/sql"
	stderrors "errors"
	"fmt"
	"sync"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/runpod"
	"github.com/osteele/weft/internal/vastai"
)

// TerminateInstancesParallel terminates multiple cloud instances in parallel.
func TerminateInstancesParallel(database *sql.DB, instanceIDs []int64) (int, []error) {
	return terminateInstancesParallel(database, instanceIDs, newCloudClientForStoredProvider)
}

type storedProviderClientFactory func(string) (cloud.Client, error)

// EmptyLaunchCancellation reports whether stopping a job made its launch
// empty and whether the provider resource was destroyed immediately.
type EmptyLaunchCancellation struct {
	Requested bool
	Destroyed bool
}

// CancelLaunchIfNoActiveJobs durably cancels a launch when every job still
// assigned to it is terminal. A launch whose provider ID is not known yet
// keeps the destroy intent pending; the launch path or reconciler completes it
// as soon as provider identity becomes available.
func CancelLaunchIfNoActiveJobs(database *sql.DB, instanceID int64) (EmptyLaunchCancellation, error) {
	return cancelLaunchIfNoActiveJobs(database, instanceID, newCloudClientForStoredProvider)
}

func cancelLaunchIfNoActiveJobs(database *sql.DB, instanceID int64, clientForProvider storedProviderClientFactory) (EmptyLaunchCancellation, error) {
	const detail = "all assigned jobs were stopped before the rental could do more work"
	intent, requested, err := db.RecordEmptyLaunchDestroyIntent(
		database, instanceID, db.LaunchStatusCancelled, db.TerminationReasonCancelled, detail,
	)
	if stderrors.Is(err, db.ErrLaunchTerminal) {
		return EmptyLaunchCancellation{}, nil
	}
	if err != nil || !requested {
		return EmptyLaunchCancellation{}, err
	}
	result := EmptyLaunchCancellation{Requested: true}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		return result, fmt.Errorf("reload instance %s after cancellation request: %w", ids.FormatInstanceID(instanceID), err)
	}
	if ci == nil {
		return result, fmt.Errorf("instance %s disappeared after cancellation request", ids.FormatInstanceID(instanceID))
	}
	providerID := ci.EffectiveProviderID()
	if providerID == "" {
		return result, nil
	}
	client, err := clientForProvider(ci.Provider)
	if err != nil {
		intent.LastError = err.Error()
		_ = db.UpdateLaunchTerminationIntent(database, instanceID, intent)
		return result, fmt.Errorf("resolve provider for empty instance %s: %w", ids.FormatInstanceID(instanceID), err)
	}
	if err := client.DestroyInstance(providerID); err != nil && !stderrors.Is(err, cloud.ErrInstanceNotFound) {
		intent.LastError = err.Error()
		_ = db.UpdateLaunchTerminationIntent(database, instanceID, intent)
		return result, fmt.Errorf("destroy empty %s instance %s: %w", ci.Provider, providerID, err)
	}
	intent.State = instanceintent.StateSucceeded
	intent.DestroySucceededAtUnix = time.Now().Unix()
	intent.ProviderInstanceID = providerID
	if _, err := db.ApplyLaunchTerminalTransition(database, instanceID, db.LaunchTerminalTransition{
		Status:               db.LaunchStatusCancelled,
		TerminationReason:    db.TerminationReasonCancelled,
		TerminationDetail:    detail,
		ResetJobs:            true,
		AttemptOutcome:       db.AttemptOutcomeCancelled,
		TerminationRequested: true,
		TerminationIntent:    intent,
	}); err != nil {
		return result, fmt.Errorf("record empty instance %s canceled: %w", ids.FormatInstanceID(instanceID), err)
	}
	result.Destroyed = true
	return result, nil
}

func terminateInstancesParallel(database *sql.DB, instanceIDs []int64, clientForProvider storedProviderClientFactory) (int, []error) {
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
			if providerInstID == "" {
				mu.Lock()
				errors = append(errors, fmt.Errorf("destroy instance %s: provider instance ID is unknown", ids.FormatInstanceID(instanceID)))
				mu.Unlock()
				return
			}
			client, err := clientForProvider(ci.Provider)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("resolve provider for instance %s: %w", ids.FormatInstanceID(instanceID), err))
				mu.Unlock()
				return
			}
			intent, err := db.RecordLaunchDestroyIntent(database, instanceID, db.LaunchStatusCancelled, db.TerminationReasonCancelled)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("record destroy intent for instance %s: %w", ids.FormatInstanceID(instanceID), err))
				mu.Unlock()
				return
			}
			if err := client.DestroyInstance(providerInstID); err != nil && !stderrors.Is(err, cloud.ErrInstanceNotFound) {
				mu.Lock()
				errors = append(errors, fmt.Errorf("destroy %s instance %s: %w", ci.Provider, providerInstID, err))
				mu.Unlock()
				return
			}
			intent.State = instanceintent.StateSucceeded
			intent.DestroySucceededAtUnix = time.Now().Unix()

			if _, err := db.ApplyLaunchTerminalTransition(database, instanceID, db.LaunchTerminalTransition{
				Status:               db.LaunchStatusCancelled,
				TerminationReason:    db.TerminationReasonCancelled,
				ResetJobs:            true,
				AttemptOutcome:       db.AttemptOutcomeCancelled,
				TerminationRequested: true,
				TerminationIntent:    intent,
			}); err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("record terminated instance %s: %w", ids.FormatInstanceID(instanceID), err))
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

func newCloudClientForStoredProvider(provider string) (cloud.Client, error) {
	switch cloud.Provider(provider) {
	case cloud.ProviderVastai:
		return vastai.NewCloudClient(vastai.NewClient()), nil
	case cloud.ProviderRunpod:
		return runpod.NewCloudClient(), nil
	default:
		return nil, fmt.Errorf("unsupported cloud provider %q", provider)
	}
}
