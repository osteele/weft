package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

// ReconcileResult holds the outcome of a reconciliation pass.
type ReconcileResult struct {
	Reconciled          int     // total number of instances whose state changed
	TerminatedInstances []int64 // DB IDs of instances that were moved to a terminal state
}

// minDeadConfirmTime is how long an instance must continuously appear dead
// before we declare it terminal. With transient API errors now skipping
// reconciliation (rather than treating as dead), a shorter window is safe.
const minDeadConfirmTime = 30 * time.Second

// Reconciler runs reconciliation passes and remembers when each instance was
// first seen dead, so we can require a sustained dead period before terminating.
type Reconciler struct {
	mu                 sync.Mutex
	firstDeadAt        map[int64]time.Time // keyed by Launch.ID
	probeFailures      map[int64]probeFailureState
	lastProviderStatus map[int64]string // last observed provider status per instance
	deadConfirmTime    time.Duration    // 0 uses minDeadConfirmTime
}

type probeFailureState struct {
	FirstAt time.Time
	Count   int
}

// NewReconciler creates a Reconciler ready for use.
func NewReconciler() *Reconciler {
	return &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
	}
}

var (
	fetchReconcileHeartbeat = fetchHeartbeat
	probeCampaignAgent      = func(inst *cloud.Instance, timeout time.Duration) (bool, error) {
		out, err := cloud.RunOnInstance(inst, "pgrep -af 'weft-agent .*run-campaign'", timeout)
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(out) != "", nil
	}
	fetchReconcileTerminationIntent = fetchTerminationIntentFromR2
)

func (r *Reconciler) confirmTime() time.Duration {
	if r.deadConfirmTime == 0 {
		return minDeadConfirmTime
	}
	return r.deadConfirmTime
}

// ReconcileLaunches checks all running/launching instances against the
// cloud provider and marks dead ones as failed (or completed if R2 has
// a completion marker). r2Client may be nil, in which case completion detection is skipped.
func (r *Reconciler) ReconcileLaunches(database *sql.DB, clients []cloud.Client, r2Client *r2.Client) (*ReconcileResult, error) {
	instances, err := db.ListRunningLaunches(database)
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
	for id := range r.probeFailures {
		if !activeIDs[id] {
			delete(r.probeFailures, id)
		}
	}
	for id := range r.lastProviderStatus {
		if !activeIDs[id] {
			delete(r.lastProviderStatus, id)
		}
	}
	r.mu.Unlock()

	// Batch-fetch all provider instances once per reconciliation pass.
	// This replaces N individual ShowInstance() calls with one ListAllInstances()
	// per provider, eliminating transient-error-per-instance problems.
	providerInstances := batchFetchProviderInstances(clients)

	result := &ReconcileResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, ci := range instances {
		wg.Add(1)
		go func(ci *db.Launch) {
			defer wg.Done()
			reconciled, terminated := r.reconcileOneInstance(database, clients, r2Client, ci, providerInstances)
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
	recentlyTerminal, err := db.ListRecentlyTerminalLaunches(database, 30*time.Minute)
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
		go func(ci *db.Launch, providerID string, client cloud.Client) {
			defer wg2.Done()
			inst, err := client.ShowInstance(providerID)
			if errors.Is(err, cloud.ErrInstanceNotFound) {
				if markTerminationIntentDestroyed(database, ci, time.Now()) {
					mu.Lock()
					result.Reconciled++
					mu.Unlock()
				}
				return
			}
			if err != nil {
				return // can't check — skip
			}
			if isProviderTerminal(inst) {
				if markTerminationIntentDestroyed(database, ci, time.Now()) {
					mu.Lock()
					result.Reconciled++
					mu.Unlock()
				}
				return
			}
			log.Printf("reconcile: safety-net destroying leaked provider instance %s (db instance %d, status %s)",
				providerID, ci.ID, ci.Status)
			if err := client.DestroyInstance(providerID); err != nil {
				log.Printf("reconcile: safety-net destroy %s failed: %v", providerID, err)
			}
			mu.Lock()
			result.Reconciled++
			mu.Unlock()
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
// providerInstances is the batch-fetched map from batchFetchProviderInstances.
func (r *Reconciler) reconcileOneInstance(database *sql.DB, clients []cloud.Client, r2Client *r2.Client, ci *db.Launch, providerInstances map[string]map[string]*cloud.Instance) (bool, bool) {
	// Check for grace-wait state for running instances via R2.
	// This is handled outside CheckInstance because it writes to the DB as a side effect.
	if ci.Status == db.LaunchStatusRunning && r2Client != nil {
		if graceDetected := checkR2GraceStatus(r2Client, ci, database); graceDetected {
			return true, false // reconciled but not terminal (grace is not terminal)
		}
	}

	// Look up provider instance state from the batch-fetched map.
	providerID := ci.EffectiveProviderID()
	var inst *cloud.Instance
	var providerErr error
	client := clientForProvider(clients, cloud.Provider(ci.Provider))
	if providerID != "" && client != nil {
		providerKey := ci.Provider
		if byProvider, ok := providerInstances[providerKey]; ok {
			if cached, found := byProvider[providerID]; found {
				inst = cached
			}
			// Not in batch results → instance is gone from provider
		} else {
			// Batch fetch failed for this provider — fall back to individual call
			inst, providerErr = client.ShowInstance(providerID)
			if providerErr != nil {
				if errors.Is(providerErr, cloud.ErrInstanceNotFound) {
					providerErr = nil
				} else {
					log.Printf("reconcile: ShowInstance(%s) for instance %d: %v (skipping)", providerID, ci.ID, providerErr)
					return false, false
				}
			}
		}
	}

	if inst != nil {
		r.recordProviderStatusTransition(database, ci.ID, inst.Status)
	}

	// Fetch and persist termination intent from R2
	intent, intentErr := fetchReconcileTerminationIntent(context.Background(), r2Client, ci.ID)
	if intentErr != nil {
		log.Printf("reconcile: fetch termination intent for instance %d: %v", ci.ID, intentErr)
	}
	if intent != nil {
		if err := db.UpdateLaunchTerminationIntent(database, ci.ID, intent); err != nil {
			log.Printf("reconcile: persist termination intent for instance %d: %v", ci.ID, err)
		}
	} else if ci.TerminationIntent != nil {
		intent = ci.TerminationIntent
	}

	jobs, _ := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
	jobState := ComputeJobState(jobs)

	// Fetch R2 phase markers so bootstrap-stall check has current data.
	// Without this, the reconciler sees empty InstancePhase and may kill
	// instances that are actually running jobs.
	var instancePhase, bootstrapStage string
	if r2Client != nil && ci.Status == db.LaunchStatusRunning && ci.LaunchedAt != nil {
		ctx := context.Background()
		instancePhase = fetchInstancePhase(ctx, r2Client, ci.ID)
		if instancePhase != "" {
			if verb, phaseJobID, ok := ParsePhaseJobID(instancePhase); ok && phaseJobID > 0 {
				switch verb {
				case PhaseRunning, PhaseUploading, PhaseUploadingResults, PhaseFinalizing:
					if j := findJobInSlice(jobs, phaseJobID); j != nil && j.Status == db.StatusQueued {
						if err := db.MarkQueuedJobRunning(database, phaseJobID); err != nil {
							log.Printf("reconcile: mark job %d running from R2 phase: %v", phaseJobID, err)
						}
						jobState.HasStartedJob = true
					}
				}
			}
		} else if !jobState.HasStartedJob {
			bootstrapStage = fetchBootstrapStage(ctx, r2Client, ci.ID)
		}
	}

	now := time.Now()
	action := r.CheckInstance(CheckInstanceParams{
		CI:                ci,
		ProviderInst:      inst,
		ProviderErr:       providerErr,
		R2Client:          r2Client,
		JobState:          jobState,
		InstancePhase:     instancePhase,
		BootstrapStage:    bootstrapStage,
		Now:               now,
		TerminationIntent: intent,
	})

	// Handle termination intent post-processing (mark destroy succeeded)
	if action.Kind == ActionTerminationIntent && isProviderTerminal(inst) {
		markTerminationIntentDestroyed(database, ci, now)
	}

	if action.Kind != ActionNone && action.Kind != ActionDisplayOnly {
		log.Printf("reconcile: instance %d action=%d (%s)", ci.ID, action.Kind, action.StallMessage)
		reconciled, terminated := ExecuteAction(database, client, ci, action)
		if terminated && r2Client != nil {
			// Verify results for completed instances via the R2 completion manifest
			if action.TerminalStatus == db.LaunchStatusCompleted {
				verifyInstanceResults(database, r2Client, ci.ID)
			}
			// Extract undeclared HF models from disk-full failures
			if action.TerminationReason == db.TerminationReasonDiskFull {
				ProcessDiskFailureReport(r2Client, ci.ID, database)
			}
		}
		return reconciled, terminated
	}

	// Stale heartbeat with SSH probe — reconciler-specific, not in CheckInstance
	if client != nil && !isProviderTerminal(inst) && ci.Status == db.LaunchStatusRunning && r2Client != nil {
		if reconciled, terminated := r.reconcileStaleHeartbeat(database, client, r2Client, ci, inst); reconciled {
			return true, terminated
		}
	}

	return false, false
}

// verifyInstanceResults reads the R2 completion manifest for a completed instance
// and sets the results_verified flag based on upload statuses.
func verifyInstanceResults(database *sql.DB, r2Client *r2.Client, instanceID int64) {
	manifest := readR2CompletionManifest(r2Client, instanceID)
	if manifest == nil {
		// Legacy marker or missing — leave results_verified as NULL
		return
	}
	verified := manifest.AllUploadsOK()
	if err := db.UpdateLaunchResultsVerified(database, instanceID, verified); err != nil {
		log.Printf("reconcile: update results_verified for instance %d: %v", instanceID, err)
		return
	}
	if !verified {
		log.Printf("reconcile: instance %d completed but uploads were partial/failed", instanceID)
	}
}

func markTerminationIntentDestroyed(database *sql.DB, ci *db.Launch, confirmedAt time.Time) bool {
	if ci == nil || !HasActiveTerminationIntent(ci.TerminationIntent) {
		return false
	}
	marker := *ci.TerminationIntent
	if marker.RequestedAtUnix == 0 && ci.TerminationRequestedAt != nil {
		marker.RequestedAtUnix = *ci.TerminationRequestedAt
	}
	if marker.DestroyStartedAtUnix == 0 {
		marker.DestroyStartedAtUnix = confirmedAt.Unix()
	}
	marker.DestroySucceededAtUnix = confirmedAt.Unix()
	if err := db.UpdateLaunchTerminationIntent(database, ci.ID, &marker); err != nil {
		log.Printf("reconcile: persist destroy success for instance %d: %v", ci.ID, err)
		return false
	}
	ci.TerminationIntent = &marker
	return true
}

const (
	minProbeFailureAttempts = 3
	minProbeFailureWindow   = 2 * time.Minute
)

func (r *Reconciler) clearProbeFailure(id int64) {
	r.mu.Lock()
	delete(r.probeFailures, id)
	r.mu.Unlock()
}

func (r *Reconciler) noteProbeFailure(id int64) probeFailureState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.probeFailures == nil {
		r.probeFailures = make(map[int64]probeFailureState)
	}
	state := r.probeFailures[id]
	if state.Count == 0 {
		state.FirstAt = time.Now()
	}
	state.Count++
	r.probeFailures[id] = state
	return state
}

func (r *Reconciler) reconcileStaleHeartbeat(database *sql.DB, client cloud.Client, r2Client *r2.Client, ci *db.Launch, inst *cloud.Instance) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	heartbeat, heartbeatAge := fetchReconcileHeartbeat(ctx, r2Client, ci.ID)
	if heartbeatAge == 0 || heartbeatAge <= heartbeatStaleThreshold {
		r.clearProbeFailure(ci.ID)
		return false, false
	}

	agentAlive, err := probeCampaignAgent(inst, 15*time.Second)
	if err == nil && agentAlive {
		r.clearProbeFailure(ci.ID)
		return false, false
	}

	if err != nil {
		state := r.noteProbeFailure(ci.ID)
		elapsed := time.Since(state.FirstAt)
		if state.Count < minProbeFailureAttempts && elapsed < minProbeFailureWindow {
			log.Printf("reconcile: instance %d heartbeat stale (%s) and agent probe is unreachable (attempt %d/%d over %s); waiting before termination",
				ci.ID, heartbeatAge.Truncate(time.Second), state.Count, minProbeFailureAttempts, elapsed.Truncate(time.Second))
			return false, false
		}
	} else {
		r.clearProbeFailure(ci.ID)
	}

	reason := failureTerminationReasonFromR2(ctx, r2Client, ci.ID, db.TerminationReasonUnknown)

	// Refine unknown failures using last heartbeat: low disk free → disk_full
	if reason == db.TerminationReasonUnknown && heartbeat != nil && heartbeat.DiskFreeBytes > 0 && heartbeat.DiskTotalBytes > 0 {
		freePercent := float64(heartbeat.DiskFreeBytes) / float64(heartbeat.DiskTotalBytes)
		if freePercent < 0.05 {
			reason = db.TerminationReasonDiskFull
		}
	}

	log.Printf("reconcile: instance %d heartbeat stale (%s) and agent probe failed/alive=%t err=%v, marking failed (%s)",
		ci.ID, heartbeatAge.Truncate(time.Second), agentAlive, err, reason)

	providerID := ci.EffectiveProviderID()
	if providerID != "" {
		if destroyErr := client.DestroyInstance(providerID); destroyErr != nil {
			log.Printf("reconcile: destroy stale-heartbeat instance %d: %v", ci.ID, destroyErr)
		}
	}

	if err := db.UpdateLaunchStatus(database, ci.ID, db.LaunchStatusFailed, reason); err != nil {
		log.Printf("reconcile: update stale-heartbeat instance %d status: %v", ci.ID, err)
		return false, false
	}
	if resetCount, err := db.ResetLaunchJobs(database, ci.ID, db.AttemptOutcomeOrphaned); err != nil {
		log.Printf("reconcile: reset jobs for stale-heartbeat instance %d: %v", ci.ID, err)
	} else if resetCount > 0 {
		log.Printf("reconcile: reset %d jobs from stale-heartbeat instance %d to unplaced", resetCount, ci.ID)
	}
	r.clearProbeFailure(ci.ID)
	return true, true
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
			if c.Status == db.CampaignStatusRunning {
				if err := db.UpdateCampaignStatus(database, c.ID, db.CampaignStatusFailed); err != nil {
					log.Printf("reconcile campaigns: update campaign %d to %s: %v", c.ID, db.CampaignStatusFailed, err)
				} else {
					log.Printf("reconcile campaigns: campaign %d had no instances, marking %s", c.ID, db.CampaignStatusFailed)
					c.Status = db.CampaignStatusFailed
					completed = append(completed, c)
				}
			}
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

		// All instances are terminal; only an all-completed campaign counts as
		// successful. Any failed/cancelled/mixed terminal outcome is a failure.
		allCompleted := true
		for _, inst := range instances {
			if inst.Status != db.LaunchStatusCompleted {
				allCompleted = false
				break
			}
		}

		status := db.CampaignStatusFailed
		if allCompleted {
			status = db.CampaignStatusCompleted
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
func checkR2GraceStatus(r2Client *r2.Client, ci *db.Launch, database *sql.DB) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := r2keys.GraceStatus(ci.ID)
	data := fetchR2Marker(ctx, r2Client, key)
	if data == "" {
		return false
	}

	var payload graceStatusPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		log.Printf("reconcile: parse grace status for instance %d: %v", ci.ID, err)
		return false
	}

	if payload.Deadline.Unix == 0 {
		return false
	}

	log.Printf("reconcile: instance %d entered grace-wait (deadline %d)", ci.ID, payload.Deadline.Unix)
	if err := db.SetLaunchGraceStarted(database, ci.ID, payload.Deadline.Unix); err != nil {
		log.Printf("reconcile: set grace for instance %d: %v", ci.ID, err)
		return false
	}
	return true
}

// hasR2CompletionMarker checks whether the wrapper wrote a completion marker
// to R2 before the instance self-destructed. Returns false if r2Client is nil.
func hasR2CompletionMarker(r2Client *r2.Client, instanceID int64) (exists bool) {
	if r2Client == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			exists = false
		}
	}()
	key := r2keys.CampaignComplete(instanceID)
	exists, err := r2Client.ObjectExists(context.Background(), key)
	return err == nil && exists
}

// readR2CompletionManifest reads and parses the R2 completion marker.
// Returns nil if the marker doesn't exist, is empty, or uses the legacy bare
// exit-code format. Only returns a manifest for the new JSON format.
func readR2CompletionManifest(r2Client *r2.Client, instanceID int64) *runner.InstanceCompletionManifest {
	if r2Client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data := fetchR2Marker(ctx, r2Client, r2keys.CampaignComplete(instanceID))
	if data == "" {
		return nil
	}
	m, err := runner.ParseCompletionMarker(data)
	if err != nil {
		log.Printf("reconcile: parse completion manifest for instance %d: %v", instanceID, err)
		return nil
	}
	return m
}

func fetchTerminationIntentFromR2(ctx context.Context, r2Client *r2.Client, instanceID int64) (_ *instanceintent.Marker, err error) {
	if r2Client == nil {
		return nil, nil
	}
	defer func() {
		if recover() != nil {
			err = nil
		}
	}()

	data := fetchR2Marker(ctx, r2Client, r2keys.InstanceTerminationIntent(instanceID))
	if data == "" {
		return nil, nil
	}

	var marker instanceintent.Marker
	if err := json.Unmarshal([]byte(data), &marker); err != nil {
		return nil, err
	}
	if marker.TerminalStatus == "" {
		return nil, nil
	}
	return &marker, nil
}

// maxEmptyStatusTime is the maximum time to wait for a provider instance to
// report a non-empty status. Instances stuck with empty actual_status beyond
// this threshold are terminated as infra failures.
const maxEmptyStatusTime = 1 * time.Minute

// maxPreRunningStatusTime is the maximum time to wait for a provider instance to
// reach "running" status. Instances stuck in any pre-running status ("created",
// "loading", etc.) beyond this threshold are terminated as infra failures.
const maxPreRunningStatusTime = 5 * time.Minute

// recordProviderStatusTransition detects when a provider instance's status
// changes and records the transition in the DB. The DB write is performed
// outside the lock to avoid holding it during I/O.
func (r *Reconciler) recordProviderStatusTransition(database *sql.DB, instanceID int64, newStatus string) {
	r.mu.Lock()
	oldStatus := r.lastProviderStatus[instanceID]
	changed := oldStatus != newStatus
	if changed {
		r.lastProviderStatus[instanceID] = newStatus
	}
	r.mu.Unlock()

	if changed && oldStatus != "" {
		_ = db.InsertProviderStatusTransition(database, instanceID, time.Now(), oldStatus, newStatus)
	}
}

// batchFetchProviderInstances calls ListAllInstances() once per provider and
// returns a nested map: provider key → provider instance ID → *cloud.Instance.
// If a provider's batch call fails, that provider is omitted from the map and
// reconcileOneInstance falls back to individual ShowInstance() calls.
func batchFetchProviderInstances(clients []cloud.Client) map[string]map[string]*cloud.Instance {
	result := make(map[string]map[string]*cloud.Instance, len(clients))
	for _, client := range clients {
		if client == nil {
			continue
		}
		instances, err := client.ListAllInstances()
		if err != nil {
			log.Printf("reconcile: batch ListAllInstances(%s) failed: %v (falling back to per-instance calls)", client.Provider(), err)
			continue
		}
		if instances == nil {
			// Provider doesn't support batch listing — fall back to per-instance calls
			continue
		}
		byID := make(map[string]*cloud.Instance, len(instances))
		for i := range instances {
			byID[instances[i].ProviderID] = &instances[i]
		}
		result[string(client.Provider())] = byID
	}
	return result
}

// isProviderTerminal returns true if the provider instance is in a terminal/dead state.
func isProviderTerminal(inst *cloud.Instance) bool {
	if inst == nil {
		return true // instance not found = dead
	}
	switch inst.Status {
	case "exited", "destroyed", "error", "dead", "stopped":
		return true
	}
	// Provider intended to stop/destroy but Status hasn't caught up yet
	// (e.g., Status still "created" while IntendedStatus is "stopped")
	if (inst.IntendedStatus == "stopped" || inst.IntendedStatus == "destroyed") &&
		inst.Status != "running" {
		return true
	}
	return false
}
