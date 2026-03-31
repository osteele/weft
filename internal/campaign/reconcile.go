package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// survivalCacheTTL controls how often adaptive survival thresholds (bootstrap,
// setup phase) are recomputed from historical data. The underlying statistics
// change slowly (only when instances complete or fail), so recomputing every
// 5 minutes is sufficient.
const survivalCacheTTL = 5 * time.Minute

// Reconciler runs reconciliation passes and remembers when each instance was
// first seen dead, so we can require a sustained dead period before terminating.
type Reconciler struct {
	mu                 sync.Mutex
	firstDeadAt        map[int64]time.Time // keyed by Launch.ID
	probeFailures      map[int64]probeFailureState
	lastProviderStatus map[int64]string // last observed provider status per instance
	deadConfirmTime    time.Duration    // 0 uses minDeadConfirmTime

	// bootstrapTimeouts caches adaptive bootstrap thresholds per provider,
	// recomputed at most once per survivalCacheTTL.
	bootstrapTimeouts   map[string]*db.BootstrapSurvival
	bootstrapTimeoutsAt time.Time // when the cache was last populated

	// setupSurvivalCache caches adaptive setup-phase thresholds keyed by
	// "command\x00workingDir", recomputed at most once per survivalCacheTTL.
	setupSurvivalCache   map[string]*db.SetupSurvival
	setupSurvivalCacheAt time.Time
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
	fetchReconcileTerminationIntent  = fetchTerminationIntentFromR2
	reconcileCheckR2GraceStatus      = checkR2GraceStatus
	reconcileCheckAndSyncJobComplete = CheckAndSyncJobComplete
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
		slog.Debug("checking running instances", "component", "reconcile", "count", len(instances))
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

	// Recompute adaptive bootstrap timeouts only when the cache has expired.
	if time.Since(r.bootstrapTimeoutsAt) >= survivalCacheTTL {
		r.bootstrapTimeouts = make(map[string]*db.BootstrapSurvival)
		providers := make(map[string]bool)
		for _, ci := range instances {
			providers[ci.Provider] = true
		}
		for provider := range providers {
			if survival, err := db.ComputeBootstrapSurvival(database, provider); err != nil {
				slog.Warn("failed to compute bootstrap survival", "component", "reconcile", "provider", provider, "error", err)
			} else {
				r.bootstrapTimeouts[provider] = survival
				slog.Debug("bootstrap thresholds computed", "component", "reconcile", "provider", provider, "warn_after", survival.WarnAfter.Truncate(time.Second), "terminate_after", survival.TerminateAfter.Truncate(time.Second), "sample_size", survival.SampleSize)
			}
		}
		r.bootstrapTimeoutsAt = time.Now()
	}

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
		slog.Warn("failed to reset orphaned cloud jobs", "component", "reconcile", "error", err)
	} else if orphaned > 0 {
		slog.Info("reset orphaned jobs from dead cloud instances", "component", "reconcile", "count", orphaned)
		mu.Lock()
		result.Reconciled += int(orphaned)
		mu.Unlock()
	}

	// Safety net: check recently-terminal instances to ensure provider instances are destroyed.
	// Catches cases where self-destruct failed or a code path marked an instance terminal
	// without calling DestroyInstance.
	recentlyTerminal, err := db.ListRecentlyTerminalLaunches(database, 30*time.Minute)
	if err != nil {
		slog.Warn("failed to list recently terminal instances", "component", "reconcile", "error", err)
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
			slog.Warn("safety-net destroying leaked provider instance", "component", "reconcile", "provider", providerID, "instance", ci.ID, "status", ci.Status)
			_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
				EventKind: db.EventReconcileSafetyNetDestroy,
				LaunchID:  ci.ID,
				Detail:    fmt.Sprintf("leaked provider %s, db status %s", providerID, ci.Status),
			})
			if err := client.DestroyInstance(providerID); err != nil {
				slog.Warn("safety-net destroy failed", "component", "reconcile", "provider", providerID, "error", err)
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
		slog.Warn("orphan sweep error", "component", "reconcile", "error", sweepErr)
	} else if swept > 0 {
		slog.Info("orphan sweep destroyed instances", "component", "reconcile", "count", swept)
		_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind: db.EventReconcileOrphanSweep,
			JobCount:  swept,
		})
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
		if graceDetected := reconcileCheckR2GraceStatus(r2Client, ci, database); graceDetected {
			syncJobCompletionsFromR2(database, r2Client, ci.ID)
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
					slog.Warn("ShowInstance failed, skipping", "component", "reconcile", "provider", providerID, "instance", ci.ID, "error", providerErr)
					return false, false
				}
			}
		}
	}

	if inst != nil {
		r.recordProviderStatusTransition(database, ci.ID, inst.Status)
	}

	jobs, _ := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
	jobState := ComputeJobState(jobs)

	// Sync external state (R2 markers, termination intent) to launch_live_state.
	synced := SyncInstanceState(context.Background(), database, ci, r2Client, jobs, jobState, SyncInstanceStateOpts{})

	now := time.Now()

	// Setup survival: reconciler-specific caching of per-command thresholds.
	var setupSurvival *db.SetupSurvival
	if verb, phaseJobID, ok := ParsePhaseJobID(synced.InstancePhase); ok && verb == PhaseSetup {
		if j := findJobInSlice(jobs, phaseJobID); j != nil {
			setupSurvival = r.getSetupSurvival(database, j.Command, j.WorkingDir)
		}
	}

	params := synced.CheckParams(ci, r2Client, jobState, now)
	params.ProviderInst = inst
	params.ProviderErr = providerErr
	params.SetupSurvival = setupSurvival
	if survival, ok := r.bootstrapTimeouts[ci.Provider]; ok {
		params.BootstrapSurvival = survival
	}
	action := r.CheckInstance(params)

	// Handle termination intent post-processing (mark destroy succeeded)
	if action.Kind == ActionTerminationIntent && isProviderTerminal(inst) {
		markTerminationIntentDestroyed(database, ci, now)
	}

	if action.Kind != ActionNone && action.Kind != ActionDisplayOnly {
		slog.Info("reconcile action triggered", "component", "reconcile", "instance", ci.ID, "action", action.Kind, "message", action.StallMessage)

		// Before executing a terminal action, sync per-job completions from R2.
		// The agent writes .complete markers for each job; without this sync,
		// jobs that completed successfully may be marked "dead" or re-queued.
		if IsInstanceTerminal(action.TerminalStatus) && r2Client != nil {
			syncJobCompletionsFromR2(database, r2Client, ci.ID)
		}

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

// syncJobCompletionsFromR2 checks R2 for per-job .complete markers and records
// any completions in the DB. This ensures that jobs which finished successfully
// are in terminal status before ExecuteAction resets or closes attempts.
func syncJobCompletionsFromR2(database *sql.DB, r2Client *r2.Client, instanceID int64) {
	jobs, _ := db.GetLaunchJobsIncludingAttempts(database, instanceID)
	ctx := context.Background()
	var synced, remaining int
	for _, j := range jobs {
		if !db.IsTerminalStatus(j.Status) {
			if reconcileCheckAndSyncJobComplete(ctx, r2Client, database, j.ID) {
				synced++
			} else {
				remaining++
			}
		}
	}
	if synced > 0 || remaining > 0 {
		slog.Info("synced job completions from R2",
			"component", "reconcile", "instance", instanceID,
			"synced", synced, "remaining_non_terminal", remaining)
	}
}

// getSetupSurvival returns cached setup survival thresholds for a command+workdir,
// recomputing when the cache has expired.
func (r *Reconciler) getSetupSurvival(database *sql.DB, command, workingDir string) *db.SetupSurvival {
	key := command + "\x00" + workingDir

	// Check cache under lock.
	r.mu.Lock()
	if time.Since(r.setupSurvivalCacheAt) >= survivalCacheTTL {
		r.setupSurvivalCache = make(map[string]*db.SetupSurvival)
		r.setupSurvivalCacheAt = time.Now()
	}
	if s, ok := r.setupSurvivalCache[key]; ok {
		r.mu.Unlock()
		return s
	}
	r.mu.Unlock()

	// Compute outside lock to avoid blocking other reconciler operations.
	s, err := db.ComputeSetupSurvival(database, command, workingDir)
	if err != nil {
		slog.Warn("failed to compute setup survival", "component", "reconcile", "command", command, "working_dir", workingDir, "error", err)
		return nil
	}

	r.mu.Lock()
	r.setupSurvivalCache[key] = s
	r.mu.Unlock()
	return s
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
		slog.Warn("failed to update results_verified", "component", "reconcile", "instance", instanceID, "error", err)
		return
	}
	if !verified {
		slog.Warn("instance completed but uploads were partial/failed", "component", "reconcile", "instance", instanceID)
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
		slog.Warn("failed to persist destroy success", "component", "reconcile", "instance", ci.ID, "error", err)
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
			slog.Debug("heartbeat stale and agent probe unreachable, waiting before termination", "component", "reconcile", "instance", ci.ID, "heartbeat_age", heartbeatAge.Truncate(time.Second), "attempt", state.Count, "max_attempts", minProbeFailureAttempts, "elapsed", elapsed.Truncate(time.Second))
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

	slog.Warn("marking instance failed due to stale heartbeat", "component", "reconcile", "instance", ci.ID, "heartbeat_age", heartbeatAge.Truncate(time.Second), "agent_alive", agentAlive, "probe_error", err, "reason", reason)

	providerID := ci.EffectiveProviderID()
	if providerID != "" {
		if destroyErr := client.DestroyInstance(providerID); destroyErr != nil {
			slog.Warn("failed to destroy stale-heartbeat instance", "component", "reconcile", "instance", ci.ID, "error", destroyErr)
		}
	}

	detail := fmt.Sprintf("heartbeat stale %s, reason=%s", heartbeatAge.Truncate(time.Second), reason)
	if err := db.UpdateLaunchStatus(database, ci.ID, db.LaunchStatusFailed, reason, detail); err != nil {
		slog.Warn("failed to update stale-heartbeat instance status", "component", "reconcile", "instance", ci.ID, "error", err)
		return false, false
	}
	_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventReconcileStaleHeartbeat,
		LaunchID:  ci.ID,
		GPUSpec:   ci.GPUSpec,
		Detail:    detail,
	})
	if resetCount, err := db.ResetLaunchJobs(database, ci.ID, db.AttemptOutcomeOrphaned); err != nil {
		slog.Warn("failed to reset jobs for stale-heartbeat instance", "component", "reconcile", "instance", ci.ID, "error", err)
	} else if resetCount > 0 {
		slog.Info("reset jobs from stale-heartbeat instance to unplaced", "component", "reconcile", "count", resetCount, "instance", ci.ID)
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
			slog.Warn("failed to get instances for campaign", "component", "reconcile", "campaign", c.ID, "error", err)
			continue
		}
		if len(instances) == 0 {
			if c.Status == db.CampaignStatusRunning {
				if err := db.UpdateCampaignStatus(database, c.ID, db.CampaignStatusFailed); err != nil {
					slog.Warn("failed to update campaign status", "component", "reconcile", "campaign", c.ID, "status", db.CampaignStatusFailed, "error", err)
				} else {
					slog.Info("campaign had no instances, marking failed", "component", "reconcile", "campaign", c.ID, "status", db.CampaignStatusFailed)
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
			slog.Warn("failed to update campaign status", "component", "reconcile", "campaign", c.ID, "status", status, "error", err)
		} else {
			slog.Info("campaign transitioned to terminal state", "component", "reconcile", "campaign", c.ID, "status", status)
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
		slog.Warn("failed to parse grace status", "component", "reconcile", "instance", ci.ID, "error", err)
		return false
	}

	if payload.Deadline.Unix == 0 {
		return false
	}

	slog.Info("instance entered grace-wait", "component", "reconcile", "instance", ci.ID, "deadline", payload.Deadline.Unix)
	if err := db.SetLaunchGraceStarted(database, ci.ID, payload.Deadline.Unix); err != nil {
		slog.Warn("failed to set grace for instance", "component", "reconcile", "instance", ci.ID, "error", err)
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
		slog.Warn("failed to parse completion manifest", "component", "reconcile", "instance", instanceID, "error", err)
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

	if changed {
		_ = db.RecordProviderStatus(database, instanceID, time.Now(), oldStatus, newStatus)
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
			slog.Warn("batch ListAllInstances failed, falling back to per-instance calls", "component", "reconcile", "provider", client.Provider(), "error", err)
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
	case cloud.ProviderStatusExited, cloud.ProviderStatusDestroyed, cloud.ProviderStatusError, cloud.ProviderStatusDead, cloud.ProviderStatusStopped:
		return true
	}
	// Provider intended to stop/destroy but Status hasn't caught up yet
	// (e.g., Status still "created" while IntendedStatus is "stopped")
	if (inst.IntendedStatus == cloud.ProviderStatusStopped || inst.IntendedStatus == cloud.ProviderStatusDestroyed) &&
		inst.Status != cloud.ProviderStatusRunning {
		return true
	}
	return false
}
