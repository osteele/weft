package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
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
		// Check for grace-wait state for running instances via R2
		if ci.Status == db.CloudInstanceStatusRunning && r2Client != nil {
			if graceDetected := checkR2GraceStatus(r2Client, ci, database); graceDetected {
				reconciled++
				continue
			}
		}

		// Clean up completed donor instances
		if ci.InstanceRole == "donor" && ci.Status == db.CloudInstanceStatusRunning && r2Client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			exists, _ := r2Client.ObjectExists(ctx, r2keys.DonorReady(ci.ID))
			cancel()
			if exists {
				providerID := ci.EffectiveProviderID()
				if providerID != "" {
					if client := clientForProvider(clients, cloud.Provider(ci.Provider)); client != nil {
						if err := client.DestroyInstance(providerID); err != nil {
							log.Printf("reconcile: failed to destroy completed donor %d: %v", ci.ID, err)
						}
					}
				}
				_ = db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusCompleted, db.TerminationReasonCompleted)
				reconciled++
				continue
			}
		}

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

		// Detect wedged instances: provider says "running" but no bootstrap progress
		if !isProviderTerminal(inst) && ci.Status == db.CloudInstanceStatusRunning && r2Client != nil {
			if isBootstrapStalled(r2Client, ci) {
				log.Printf("reconcile: instance %d is wedged (no bootstrap progress after %s), terminating",
					ci.ID, time.Since(time.Unix(*ci.LaunchedAt, 0)).Truncate(time.Second))

				// Try to terminate the provider instance
				if destroyErr := client.DestroyInstance(providerID); destroyErr != nil {
					log.Printf("reconcile: failed to destroy wedged instance %d: %v", ci.ID, destroyErr)
				}
				if err := db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure); err != nil {
					log.Printf("reconcile: update instance %d status: %v", ci.ID, err)
					continue
				}
				if resetCount, err := db.ResetCloudInstanceJobs(database, ci.ID, db.AttemptOutcomeOrphaned); err != nil {
					log.Printf("reconcile: reset jobs for instance %d: %v", ci.ID, err)
				} else if resetCount > 0 {
					log.Printf("reconcile: reset %d jobs from wedged instance %d to unplaced", resetCount, ci.ID)
				}
				reconciled++
				continue
			}
		}

		// If provider reports terminal state (or instance is gone) but DB doesn't, reconcile.
		// Skip grace-period instances — a transient API failure shouldn't kill the session.
		if isProviderTerminal(inst) && !IsInstanceTerminal(ci.Status) && ci.Status != db.CloudInstanceStatusGrace {
			status := "not found"
			if inst != nil {
				status = inst.Status
			}

			// Check R2 for completion marker before assuming failure.
			if hasR2CompletionMarker(r2Client, ci.ID) {
				log.Printf("reconcile: instance %d completed (R2 marker found), marking completed", ci.ID)
				if err := db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusCompleted, db.TerminationReasonCompleted); err != nil {
					log.Printf("reconcile: update instance %d status: %v", ci.ID, err)
				}
				reconciled++
				continue
			}

			// Provider dead + had been launched (running) → preempted
			reason := db.TerminationReasonPreempted
			if ci.LaunchedAt == nil {
				reason = db.TerminationReasonInfraFailure
			}

			log.Printf("reconcile: instance %d (provider %s) is dead (provider status: %s), marking failed (%s)", ci.ID, providerID, status, reason)

			if err := db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusFailed, reason); err != nil {
				log.Printf("reconcile: update instance %d status: %v", ci.ID, err)
				continue
			}
			if resetCount, err := db.ResetCloudInstanceJobs(database, ci.ID, db.AttemptOutcomeOrphaned); err != nil {
				log.Printf("reconcile: reset jobs for instance %d: %v", ci.ID, err)
			} else if resetCount > 0 {
				log.Printf("reconcile: reset %d jobs from instance %d to unplaced", resetCount, ci.ID)
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

// graceStatusPayload is the JSON structure written by the agent's grace-wait to R2.
type graceStatusPayload struct {
	State      string        `json:"state"`
	Deadline   deadlineValue `json:"deadline"`
	FailedJobs []int         `json:"failed_jobs"`
}

// deadlineValue handles both RFC3339 string and unix epoch int64 formats for the deadline field.
type deadlineValue struct {
	Unix int64
}

func (d *deadlineValue) UnmarshalJSON(data []byte) error {
	// Try int64 first
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		d.Unix = n
		return nil
	}
	// Try RFC3339 string
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("deadline must be int64 or RFC3339 string, got %s", string(data))
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("invalid deadline string %q: %w", s, err)
	}
	d.Unix = t.Unix()
	return nil
}

// checkR2GraceStatus checks R2 for a grace status marker for a running instance.
// If found, transitions the DB instance to grace state. Returns true if grace was detected.
func checkR2GraceStatus(r2Client *r2.Client, ci *db.CloudInstance, database *sql.DB) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := r2keys.GraceStatus(ci.ID)
	data, err := r2Client.GetObject(ctx, key)
	if err != nil {
		return false
	}

	var payload graceStatusPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("reconcile: parse grace status for instance %d: %v", ci.ID, err)
		return false
	}

	if payload.Deadline.Unix == 0 {
		return false
	}

	log.Printf("reconcile: instance %d entered grace-wait (deadline %d)", ci.ID, payload.Deadline.Unix)
	if err := db.SetCloudInstanceGraceStarted(database, ci.ID, payload.Deadline.Unix); err != nil {
		log.Printf("reconcile: set grace for instance %d: %v", ci.ID, err)
		return false
	}
	return true
}

// hasR2CompletionMarker checks whether the wrapper wrote a completion marker
// to R2 before the instance self-destructed. Returns false if r2Client is nil.
func hasR2CompletionMarker(r2Client *r2.Client, instanceID int64) bool {
	if r2Client == nil {
		return false
	}
	key := r2keys.CampaignComplete(instanceID)
	exists, err := r2Client.ObjectExists(context.Background(), key)
	return err == nil && exists
}

// maxBootstrapInitTime is the maximum time to wait for the first bootstrap stage marker.
// If no marker appears after this duration, the instance is considered wedged.
const maxBootstrapInitTime = 15 * time.Minute

// isBootstrapStalled returns true if a running instance has no bootstrap progress
// within the expected timeframe. Requires r2Client and a non-nil LaunchedAt.
func isBootstrapStalled(r2Client *r2.Client, ci *db.CloudInstance) bool {
	if r2Client == nil || ci.LaunchedAt == nil {
		return false
	}

	age := time.Since(time.Unix(*ci.LaunchedAt, 0))
	if age < maxBootstrapInitTime {
		return false // too early to declare stalled
	}

	// Check if any bootstrap stage marker exists
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stage := fetchBootstrapStage(ctx, r2Client, ci.ID)

	// No bootstrap stage at all after 15+ min → wedged
	return stage == ""
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
