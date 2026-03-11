package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// ReconcileResult holds the outcome of a reconciliation pass.
type ReconcileResult struct {
	Reconciled          int     // total number of instances whose state changed
	TerminatedInstances []int64 // DB IDs of instances that were moved to a terminal state
}

// minDeadConfirmTime is how long an instance must continuously appear dead
// before we declare it terminal. Guards against transient API blips.
const minDeadConfirmTime = 2 * time.Minute

// Reconciler runs reconciliation passes and remembers when each instance was
// first seen dead, so we can require a sustained dead period before terminating.
type Reconciler struct {
	mu              sync.Mutex
	firstDeadAt     map[int64]time.Time // keyed by CloudInstance.ID
	deadConfirmTime time.Duration       // 0 uses minDeadConfirmTime
}

// NewReconciler creates a Reconciler ready for use.
func NewReconciler() *Reconciler {
	return &Reconciler{firstDeadAt: make(map[int64]time.Time)}
}

func (r *Reconciler) confirmTime() time.Duration {
	if r.deadConfirmTime == 0 {
		return minDeadConfirmTime
	}
	return r.deadConfirmTime
}

// ReconcileCloudInstances checks all running/launching instances against the
// cloud provider and marks dead ones as failed (or completed if R2 has
// a completion marker). r2Client may be nil, in which case completion detection is skipped.
func (r *Reconciler) ReconcileCloudInstances(database *sql.DB, clients []cloud.Client, r2Client *r2.Client) (*ReconcileResult, error) {
	instances, err := db.ListRunningCloudInstances(database)
	if err != nil {
		return nil, err
	}

	if len(instances) > 0 {
		log.Printf("reconcile: checking %d running instances...", len(instances))
	}

	// Collect IDs of instances still running, to prune stale firstDeadAt entries.
	activeIDs := make(map[int64]bool, len(instances))
	for _, ci := range instances {
		activeIDs[ci.ID] = true
	}
	r.mu.Lock()
	for id := range r.firstDeadAt {
		if !activeIDs[id] {
			delete(r.firstDeadAt, id)
		}
	}
	r.mu.Unlock()

	result := &ReconcileResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, ci := range instances {
		wg.Add(1)
		go func(ci *db.CloudInstance) {
			defer wg.Done()
			reconciled, terminated := r.reconcileOneInstance(database, clients, r2Client, ci)
			if reconciled {
				mu.Lock()
				result.Reconciled++
				if terminated {
					result.TerminatedInstances = append(result.TerminatedInstances, ci.ID)
				}
				mu.Unlock()
			}
		}(ci)
	}
	wg.Wait()

	// Catch-all: reset jobs stranded on dead cloud instances (stale host field).
	if orphaned, err := db.ResetOrphanedCloudJobs(database); err != nil {
		log.Printf("reconcile: reset orphaned cloud jobs: %v", err)
	} else if orphaned > 0 {
		log.Printf("reconcile: reset %d orphaned jobs from dead cloud instances", orphaned)
		mu.Lock()
		result.Reconciled += int(orphaned)
		mu.Unlock()
	}

	// Safety net: check recently-terminal instances to ensure provider instances are destroyed.
	// Catches cases where self-destruct failed or a code path marked an instance terminal
	// without calling DestroyInstance.
	recentlyTerminal, err := db.ListRecentlyTerminalCloudInstances(database, 30*time.Minute)
	if err != nil {
		log.Printf("reconcile: list recently terminal instances: %v", err)
	}
	var wg2 sync.WaitGroup
	for _, ci := range recentlyTerminal {
		providerID := ci.EffectiveProviderID()
		client := clientForProvider(clients, cloud.Provider(ci.Provider))
		if client == nil {
			continue
		}

		wg2.Add(1)
		go func(ci *db.CloudInstance, providerID string, client cloud.Client) {
			defer wg2.Done()
			inst, err := client.ShowInstance(providerID)
			if err != nil {
				return // can't check — skip
			}
			if !isProviderTerminal(inst) {
				log.Printf("reconcile: safety-net destroying leaked provider instance %s (db instance %d, status %s)",
					providerID, ci.ID, ci.Status)
				if err := client.DestroyInstance(providerID); err != nil {
					log.Printf("reconcile: safety-net destroy %s failed: %v", providerID, err)
				}
				mu.Lock()
				result.Reconciled++
				mu.Unlock()
			}
		}(ci, providerID, client)
	}
	wg2.Wait()

	// Orphan sweep: destroy weft-labeled instances that belong to non-active campaigns.
	// Rate-limited to avoid excessive provider API calls.
	if swept, sweepErr := MaybeSweepOrphanedInstances(database, clients); sweepErr != nil {
		log.Printf("reconcile: orphan sweep error: %v", sweepErr)
	} else if swept > 0 {
		log.Printf("reconcile: orphan sweep destroyed %d instances", swept)
		result.Reconciled += swept
	}

	return result, nil
}

// reconcileOneInstance processes a single cloud instance for reconciliation.
// Returns (reconciled, terminated) where reconciled means state changed and
// terminated means the instance was moved to a terminal state (failed/completed).
func (r *Reconciler) reconcileOneInstance(database *sql.DB, clients []cloud.Client, r2Client *r2.Client, ci *db.CloudInstance) (bool, bool) {
	// Check for grace-wait state for running instances via R2
	if ci.Status == db.CloudInstanceStatusRunning && r2Client != nil {
		if graceDetected := checkR2GraceStatus(r2Client, ci, database); graceDetected {
			return true, false // reconciled but not terminal (grace is not terminal)
		}
	}

	// Expire grace-period instances whose deadline has passed
	if ci.Status == db.CloudInstanceStatusGrace && ci.GraceDeadline != nil && time.Now().Unix() > *ci.GraceDeadline {
		log.Printf("reconcile: instance %d grace period expired, destroying and marking failed", ci.ID)

		providerID := ci.EffectiveProviderID()
		if providerID != "" {
			if client := clientForProvider(clients, cloud.Provider(ci.Provider)); client != nil {
				if destroyErr := client.DestroyInstance(providerID); destroyErr != nil {
					log.Printf("reconcile: failed to destroy expired grace instance %d: %v", ci.ID, destroyErr)
				}
			}
		}

		if err := db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusFailed, db.TerminationReasonJobFailure); err != nil {
			log.Printf("reconcile: update instance %d status: %v", ci.ID, err)
			return false, false
		}
		if resetCount, err := db.ResetCloudInstanceJobs(database, ci.ID, db.AttemptOutcomeOrphaned); err != nil {
			log.Printf("reconcile: reset jobs for instance %d: %v", ci.ID, err)
		} else if resetCount > 0 {
			log.Printf("reconcile: reset %d jobs from expired grace instance %d to unplaced", resetCount, ci.ID)
		}
		return true, true
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
			return true, true
		}
	}

	providerID := ci.EffectiveProviderID()
	if providerID == "" {
		return false, false
	}

	client := clientForProvider(clients, cloud.Provider(ci.Provider))
	if client == nil {
		return false, false
	}

	inst, err := client.ShowInstance(providerID)
	if err != nil && !errors.Is(err, cloud.ErrInstanceNotFound) {
		log.Printf("reconcile: ShowInstance(%s) for instance %d: %v", providerID, ci.ID, err)
		return false, false
	}

	// Detect instances stuck with empty provider status (never started).
	// Do NOT add "" to isProviderTerminal — that's used in the safety-net loop
	// where treating empty as terminal would destroy provisioning instances.
	if inst != nil && inst.Status == "" && ci.LaunchedAt != nil {
		age := time.Since(time.Unix(*ci.LaunchedAt, 0))
		if age > maxEmptyStatusTime {
			log.Printf("reconcile: instance %d has empty provider status after %s, terminating",
				ci.ID, age.Truncate(time.Second))

			if destroyErr := client.DestroyInstance(providerID); destroyErr != nil {
				log.Printf("reconcile: failed to destroy empty-status instance %d: %v", ci.ID, destroyErr)
			}
			if err := db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure); err != nil {
				log.Printf("reconcile: update instance %d status: %v", ci.ID, err)
				return false, false
			}
			if resetCount, err := db.ResetCloudInstanceJobs(database, ci.ID, db.AttemptOutcomeOrphaned); err != nil {
				log.Printf("reconcile: reset jobs for instance %d: %v", ci.ID, err)
			} else if resetCount > 0 {
				log.Printf("reconcile: reset %d jobs from empty-status instance %d to unplaced", resetCount, ci.ID)
			}
			return true, true
		}
	}

	// Detect wedged instances: provider says "running" but no bootstrap progress
	if !isProviderTerminal(inst) && ci.Status == db.CloudInstanceStatusRunning && r2Client != nil {
		if isBootstrapStalled(r2Client, ci) {
			log.Printf("reconcile: instance %d is wedged (no bootstrap progress after %s), terminating",
				ci.ID, time.Since(time.Unix(*ci.LaunchedAt, 0)).Truncate(time.Second))

			if destroyErr := client.DestroyInstance(providerID); destroyErr != nil {
				log.Printf("reconcile: failed to destroy wedged instance %d: %v", ci.ID, destroyErr)
			}
			if err := db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure); err != nil {
				log.Printf("reconcile: update instance %d status: %v", ci.ID, err)
				return false, false
			}
			if resetCount, err := db.ResetCloudInstanceJobs(database, ci.ID, db.AttemptOutcomeOrphaned); err != nil {
				log.Printf("reconcile: reset jobs for instance %d: %v", ci.ID, err)
			} else if resetCount > 0 {
				log.Printf("reconcile: reset %d jobs from wedged instance %d to unplaced", resetCount, ci.ID)
			}
			return true, true
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
			// Don't reset jobs — leave them for syncCloudJobResults to update from R2.
			// Just close the attempt records.
			if err := db.CloseJobCloudAttemptsByInstance(database, ci.ID, db.AttemptOutcomeCompleted); err != nil {
				log.Printf("reconcile: close attempts for instance %d: %v", ci.ID, err)
			}
			r.mu.Lock()
			delete(r.firstDeadAt, ci.ID)
			r.mu.Unlock()
			return true, true
		}

		// Require the instance to appear dead for minDeadConfirmTime before
		// declaring it terminal. Transient API blips (false "exited" status,
		// ErrInstanceNotFound) can otherwise kill a healthy running instance.
		confirmTime := r.confirmTime()
		r.mu.Lock()
		first, seen := r.firstDeadAt[ci.ID]
		if !seen {
			r.firstDeadAt[ci.ID] = time.Now()
			r.mu.Unlock()
			if confirmTime > 0 {
				log.Printf("reconcile: instance %d (provider %s) appears dead (status: %s); waiting %s to confirm",
					ci.ID, providerID, status, confirmTime)
				return false, false
			}
		} else {
			r.mu.Unlock()
		}
		if confirmTime > 0 && time.Since(first) < confirmTime {
			log.Printf("reconcile: instance %d still appears dead (status: %s); confirming for %s more",
				ci.ID, status, (confirmTime - time.Since(first)).Truncate(time.Second))
			return false, false
		}
		r.mu.Lock()
		delete(r.firstDeadAt, ci.ID)
		r.mu.Unlock()

		// Provider dead + had been launched (running) → preempted
		reason := db.TerminationReasonPreempted
		if ci.LaunchedAt == nil {
			reason = db.TerminationReasonInfraFailure
		}

		log.Printf("reconcile: instance %d (provider %s) is dead (provider status: %s), marking failed (%s)", ci.ID, providerID, status, reason)

		if err := db.UpdateCloudInstanceStatus(database, ci.ID, db.CloudInstanceStatusFailed, reason); err != nil {
			log.Printf("reconcile: update instance %d status: %v", ci.ID, err)
			return false, false
		}
		if resetCount, err := db.ResetCloudInstanceJobs(database, ci.ID, db.AttemptOutcomeOrphaned); err != nil {
			log.Printf("reconcile: reset jobs for instance %d: %v", ci.ID, err)
		} else if resetCount > 0 {
			log.Printf("reconcile: reset %d jobs from instance %d to unplaced", resetCount, ci.ID)
		}
		return true, true
	}

	return false, false
}

// ReconcileCampaigns checks active campaigns and marks them as completed or failed
// when all their instances have reached a terminal state.
// Returns the list of campaigns that were transitioned to a terminal state.
func ReconcileCampaigns(database *sql.DB) ([]*db.Campaign, error) {
	campaigns, err := db.ListActiveCampaigns(database)
	if err != nil {
		return nil, fmt.Errorf("list active campaigns: %w", err)
	}

	var completed []*db.Campaign
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
			c.Status = status
			completed = append(completed, c)
		}
	}
	return completed, nil
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

// maxEmptyStatusTime is the maximum time to wait for a provider instance to
// report a non-empty status. Instances stuck with empty actual_status beyond
// this threshold are terminated as infra failures.
const maxEmptyStatusTime = 1 * time.Minute

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
