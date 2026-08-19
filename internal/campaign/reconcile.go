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
	"github.com/osteele/weft/internal/controlplane"
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
	JobsUpdated         int     // count of job status transitions (e.g. queued→running)
	BidsRaised          int     // interruptible bids raised after provider pause/outbid
}

// minDeadConfirmTime is how long an instance must continuously appear dead
// before we declare it terminal. With transient API errors now skipping
// reconciliation (rather than treating as dead), a shorter window is safe.
const minDeadConfirmTime = 30 * time.Second

// survivalCacheTTL controls how often historical/default survival thresholds
// (bootstrap, setup phase) are recomputed. The underlying statistics
// change slowly (only when instances complete or fail), so recomputing every
// 5 minutes is sufficient.
const survivalCacheTTL = 5 * time.Minute

// Reconciler runs reconciliation passes and remembers when each instance was
// first seen dead, so we can require a sustained dead period before terminating.
type Reconciler struct {
	mu                 sync.Mutex
	firstDeadAt        map[int64]time.Time // keyed by Launch.ID
	probeFailures      map[int64]probeFailureState
	firstUnknownAt     map[int64]time.Time // when provider status polling first started failing, keyed by Launch.ID
	lastProviderStatus map[int64]string    // last observed provider status per instance
	deadConfirmTime    time.Duration       // 0 uses minDeadConfirmTime

	// bootstrapTimeouts caches historical/default bootstrap thresholds per provider,
	// recomputed at most once per survivalCacheTTL.
	bootstrapTimeouts   map[string]*db.BootstrapSurvival
	bootstrapTimeoutsAt time.Time // when the cache was last populated

	// setupSurvivalCache caches historical/default setup-phase thresholds keyed by
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
		firstUnknownAt:     make(map[int64]time.Time),
		lastProviderStatus: make(map[int64]string),
	}
}

var (
	fetchReconcileHeartbeat = fetchHeartbeat
	probeCampaignAgent      = func(inst *cloud.Instance, timeout time.Duration) (bool, error) {
		out, err := cloud.RunOnInstance(inst, "pgrep -af 'weft-agent .*(run-instance|run-campaign)'", timeout)
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(out) != "", nil
	}
	fetchReconcileTerminationIntent     = fetchTerminationIntentFromR2
	reconcileCheckR2GraceStatus         = checkR2GraceStatus
	reconcileCheckAndSyncJobComplete    = CheckAndSyncJobComplete
	reconcileCheckAndSyncJobCompleteRun = CheckAndSyncJobCompleteRun
	// reconcileObjectExists is the R2 existence check used by the
	// dud-Vast watchdog. Stubbable so tests can drive it without a
	// real S3 backend (mirrors the fetchReconcileHeartbeat pattern).
	reconcileObjectExists = func(ctx context.Context, c *r2.Client, key string) (exists bool, err error) {
		if c == nil {
			return false, nil
		}
		// Tests pass empty &r2.Client{} stubs that nil-deref inside
		// the AWS SDK. Recover ONLY from those panics — real
		// production clients are fully populated and never panic
		// here, so any recover at runtime is a fixture artifact.
		defer func() {
			if r := recover(); r != nil {
				exists = false
				err = fmt.Errorf("r2 client panicked (likely uninitialized stub): %v", r)
			}
		}()
		return c.ObjectExists(ctx, key)
	}
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
func (r *Reconciler) ReconcileLaunches(ctx context.Context, database *sql.DB, clients []cloud.Client, r2Client *r2.Client) (*ReconcileResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return &ReconcileResult{}, nil
	}
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
	for id := range r.firstUnknownAt {
		if !activeIDs[id] {
			delete(r.firstUnknownAt, id)
		}
	}
	for id := range r.lastProviderStatus {
		if !activeIDs[id] {
			delete(r.lastProviderStatus, id)
		}
	}
	r.mu.Unlock()

	providers := make(map[string]bool)
	for _, ci := range instances {
		providers[ci.Provider] = true
	}
	r.refreshBootstrapSurvival(database, providers)

	// Batch-fetch all provider instances once per reconciliation pass.
	// This replaces N individual ShowInstance() calls with one ListAllInstances()
	// per provider, eliminating transient-error-per-instance problems.
	providerInstances := batchFetchProviderInstances(clients)

	result := &ReconcileResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	if ctx.Err() != nil {
		return result, nil
	}
	for _, ci := range instances {
		wg.Add(1)
		go func(ci *db.Launch) {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			reconciled, terminated, jobsUpdated, bidRaised := r.reconcileOneInstance(database, clients, r2Client, ci, providerInstances)
			mu.Lock()
			if reconciled {
				result.Reconciled++
				if terminated {
					result.TerminatedInstances = append(result.TerminatedInstances, ci.ID)
				}
			}
			result.JobsUpdated += jobsUpdated
			if bidRaised {
				result.BidsRaised++
			}
			mu.Unlock()
		}(ci)
	}
	wg.Wait()

	// Requeue jobs stranded on dead cloud-instance host references. Intent
	// drift (open MoveIntent / PlacementIntent rows whose placement has
	// actually landed) is now resolved by DB triggers in 00004 + 00005, not
	// by a periodic pass.
	if stale, err := db.ReconcileStaleCloudHostJobs(database, db.ReconcileStaleCloudHostJobsOptions{}); err != nil {
		slog.Warn("failed to reconcile stale cloud-host jobs", "component", "reconcile", "error", err)
	} else if len(stale.Actions) > 0 {
		slog.Debug("requeued stale cloud-host jobs", "component", "reconcile", "actions", len(stale.Actions), "jobs_updated", stale.JobsUpdated)
		mu.Lock()
		result.Reconciled += len(stale.Actions)
		result.JobsUpdated += stale.JobsUpdated
		mu.Unlock()
	}

	// Supersede stale rebalance placement reasons on jobs that fell back to the
	// unplaced queue, so `weft diagnose`/status no longer shows a failed
	// instance→instance move as the block reason for a freely-placeable job.
	if refreshed, err := db.RefreshStalePlacementReasons(database); err != nil {
		slog.Warn("failed to refresh stale placement reasons", "component", "reconcile", "error", err)
	} else if refreshed.JobsUpdated > 0 {
		slog.Debug("refreshed stale placement reasons", "component", "reconcile", "jobs_updated", refreshed.JobsUpdated)
		mu.Lock()
		result.JobsUpdated += refreshed.JobsUpdated
		mu.Unlock()
	}

	// Safety net: check recently-terminal instances to ensure provider instances are destroyed.
	// Catches cases where self-destruct failed or a code path marked an instance terminal
	// without calling DestroyInstance.
	recentlyTerminal, err := db.ListRecentlyTerminalLaunches(database, 30*time.Minute)
	if err != nil {
		slog.Warn("failed to list recently terminal instances", "component", "reconcile", "error", err)
	}
	if ctx.Err() != nil {
		return result, nil
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
			if ctx.Err() != nil {
				return
			}
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
				// Provider-terminal but still billable (e.g. "exited" on Vast.ai) — destroy it
				if needsProviderDestroy(inst) {
					slog.Debug("safety-net destroying billable terminal instance", "component", "reconcile", "provider", providerID, "instance", ci.ID, "provider_status", inst.Status)
					if err := client.DestroyInstance(providerID); err != nil {
						slog.Warn("safety-net destroy of terminal instance failed", "component", "reconcile", "provider", providerID, "instance", ci.ID, "error", err)
					}
				}
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
		slog.Debug("orphan sweep destroyed instances", "component", "reconcile", "count", swept)
		_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind: db.EventReconcileOrphanSweep,
			JobCount:  swept,
		})
		result.Reconciled += swept
	}

	// Counterpart to the orphan sweep above: retire 'planned' rows that
	// never reached the provider.
	if reaped, reapErr := MaybeSweepStalePlannedLaunches(database); reapErr != nil {
		slog.Warn("stale planned reap error", "component", "reconcile", "error", reapErr)
	} else if reaped > 0 {
		slog.Debug("stale planned reap retired launches", "component", "reconcile", "count", reaped)
		_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind: db.EventReconcileStalePlannedReap,
			JobCount:  reaped,
		})
		result.Reconciled += reaped
	}

	return result, nil
}

// reconcileOneInstance processes a single cloud instance for reconciliation.
// Returns (reconciled, terminated, jobsUpdated) where reconciled means instance
// state changed, terminated means moved to a terminal state (failed/completed),
// and jobsUpdated counts job status transitions (e.g. queued→running).
// providerInstances is the batch-fetched map from batchFetchProviderInstances.
func (r *Reconciler) reconcileOneInstance(database *sql.DB, clients []cloud.Client, r2Client *r2.Client, ci *db.Launch, providerInstances map[string]map[string]*cloud.Instance) (bool, bool, int, bool) {
	jobs, _ := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
	attemptOutcomes, _ := db.GetAttemptOutcomesByLaunch(database, ci.ID)

	// Check for grace-wait state for running instances via R2.
	// This is handled outside CheckInstance because it writes to the DB as a side effect.
	if ci.Status == db.LaunchStatusRunning && r2Client != nil {
		if hasActiveLaunchJobs(jobs, attemptOutcomes) {
			slog.Debug("ignoring grace transition while launch has active jobs", "component", "reconcile", "instance", ci.ID)
		} else if graceDetected := reconcileCheckR2GraceStatus(r2Client, ci, database); graceDetected {
			syncJobCompletionsFromR2(database, r2Client, ci.ID)
			return true, false, 0, false // reconciled but not terminal (grace is not terminal)
		}
	}

	// Look up provider instance state from the batch-fetched map.
	providerID := ci.EffectiveProviderID()
	var inst *cloud.Instance
	var providerErr error
	client := clientForProvider(clients, cloud.Provider(ci.Provider))
	if client == nil {
		providerList := make([]string, 0, len(clients))
		for _, c := range clients {
			providerList = append(providerList, string(c.Provider()))
		}
		slog.Debug("reconcile: no client for provider", "component", "reconcile", "instance", ci.ID, "ci_provider", ci.Provider, "available_clients", providerList)
	}
	if providerID != "" && client != nil {
		providerKey := ci.Provider
		if batched, ok := providerInstances[providerKey]; ok {
			inst = batched[providerID]
		}
		// Confirm batch misses via per-instance lookup. Vast.ai's list
		// endpoint omits live instances during transient API gaps;
		// trusting batch absence killed healthy rentals at the 25-min
		// watchdog. ShowInstance is authoritative.
		if inst == nil {
			showInst, showErr := client.ShowInstance(providerID)
			switch {
			case showErr == nil && showInst != nil:
				inst = showInst
			case errors.Is(showErr, cloud.ErrInstanceNotFound):
				providerErr = fmt.Errorf("provider instance %s: %w", providerID, cloud.ErrInstanceNotFound)
			case showErr != nil:
				// Transient ShowInstance failure. Keep providerErr non-nil so
				// the dead-confirm path stays gated on hysteresis rather than
				// firing on a single API blip.
				providerErr = showErr
				slog.Warn("ShowInstance failed; continuing with provider_err fallback",
					"component", "reconcile", "provider", providerID, "instance", ci.ID, "error", showErr)
			default:
				// (nil, nil) — shouldn't happen with conforming providers.
				providerErr = fmt.Errorf("provider instance %s lookup returned no data", providerID)
			}
		}
	}

	if inst != nil {
		r.recordProviderStatusTransition(database, ci.ID, inst.Status)
	}
	bidRaised := maybeRaiseInterruptibleBid(database, client, ci, inst)

	jobState := ComputeJobState(jobs, attemptOutcomes)

	// Sync external state (R2 markers, termination intent) to launch_live_state.
	synced := SyncInstanceState(context.Background(), database, ci, inst, r2Client, jobs, jobState, SyncInstanceStateOpts{})

	// Proactively sync job completions from R2 every reconcile pass.
	// Without this, jobs that completed on the instance stay "running" in the DB
	// until a terminal action triggers syncJobCompletionsFromR2, which can take hours.
	if r2Client != nil && hasActiveLaunchJobs(jobs, attemptOutcomes) {
		syncJobCompletionsFromR2(database, r2Client, ci.ID)
		// Re-fetch after syncing so downstream logic sees updated state.
		jobs, _ = db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		attemptOutcomes, _ = db.GetAttemptOutcomesByLaunch(database, ci.ID)
		jobState = ComputeJobState(jobs, attemptOutcomes)
	}

	now := time.Now()

	params := synced.CheckParams(database, ci, r2Client, jobs, attemptOutcomes, jobState,
		NewProviderObservation(inst, providerErr, r.noteProviderStatusPoll(ci.ID, inst == nil && providerErr != nil, now)),
		NewSurvivalThresholds(r.bootstrapSurvivalFor(ci.Provider), r.setupSurvivalForPhase(database, synced.InstancePhase, jobs)),
		now)
	if ci.HedgeCohortID != nil && ci.AgentReadyAtUnix == nil {
		// Gated on AgentReadyAtUnix == nil so the survivor — which by
		// definition has it set — never queries for its own cull.
		hasWinner, err := db.HedgeCohortHasReadySibling(database, *ci.HedgeCohortID, ci.ID)
		if err != nil {
			slog.Debug("hedge cohort sibling check failed", "component", "reconcile", "instance", ci.ID, "cohort", *ci.HedgeCohortID, "error", err)
		}
		params.HedgeCohortHasReadySibling = hasWinner
	}
	action := r.CheckInstance(params)

	// Handle termination intent post-processing (mark destroy succeeded).
	// Requires positive evidence the provider instance is gone; a
	// never-polled nil must not be recorded as a confirmed destroy.
	if action.Kind == ActionTerminationIntent && providerConfirmedGone(inst, providerErr) {
		if markTerminationIntentDestroyed(database, ci, now) {
			action.DestroyProvider = false
		}
	}

	if action.Kind != ActionNone && action.Kind != ActionDisplayOnly {
		reconciled, terminated := executeReconcileAction(database, client, r2Client, ci, action)
		return reconciled, terminated, synced.JobsUpdated, bidRaised
	}

	// Stale heartbeat with SSH probe — reconciler-specific, not in CheckInstance.
	// A failed provider poll defers this watchdog for interruptible launches,
	// but only up to stalePauseTimeout. Past that bound, a missing instance
	// snapshot makes the SSH probe unknown; the probe hysteresis still applies,
	// and ExecuteAction must positively destroy (or confirm absence of) the
	// provider resource before committing a terminal local transition.
	providerAllowsHeartbeatAdjudication := inst != nil && !isProviderTerminal(inst)
	if inst == nil && providerErr != nil {
		providerAllowsHeartbeatAdjudication = ci.InstanceType != cloud.InstanceTypeInterruptible ||
			errors.Is(providerErr, cloud.ErrInstanceNotFound) ||
			params.ProviderStatusUnknownFor >= stalePauseTimeout
	}
	if client != nil && providerAllowsHeartbeatAdjudication && ci.Status == db.LaunchStatusRunning && r2Client != nil {
		if hbAction := r.checkStaleHeartbeat(r2Client, ci, inst, now); hbAction.Kind != ActionNone {
			reconciled, terminated := executeReconcileAction(database, client, r2Client, ci, hbAction)
			if reconciled {
				r.clearProbeFailure(ci.ID)
				return true, terminated, synced.JobsUpdated, bidRaised
			}
		}
	}

	return bidRaised, false, synced.JobsUpdated, bidRaised
}

// executeReconcileAction runs the shared side-effect pipeline for a
// non-display reconcile action: pre-terminal completion sync from R2,
// ExecuteAction (destroy before mark), then post-terminal result
// verification and disk-failure report processing. Every path that
// terminates a launch from a reconcile pass must go through here so no
// watchdog silently skips these hooks.
func executeReconcileAction(database *sql.DB, client cloud.Client, r2Client *r2.Client, ci *db.Launch, action InstanceAction) (bool, bool) {
	slog.Debug("reconcile action triggered", "component", "reconcile", "instance", ci.ID, "action", action.Kind, "message", action.StallMessage)

	// Before executing a terminal action, sync per-job completions from R2.
	// The agent writes .complete markers for each job; without this sync,
	// jobs that completed successfully may be marked "dead" or re-queued.
	if IsInstanceTerminal(action.TerminalStatus) && r2Client != nil {
		syncJobCompletionsFromR2(database, r2Client, ci.ID)
		// The per-job marker sync above can miss a job whose .complete
		// marker is keyed by a run_id that no longer matches the DB's
		// latest attempt, or that landed after the marker scan. For a
		// completing launch the instance-level manifest is authoritative;
		// credit its exit-0 jobs so CloseLaunchAttempts does not orphan
		// and re-run work that already finished.
		if action.TerminalStatus == db.LaunchStatusCompleted {
			CreditManifestCompletions(database, r2Client, ci.ID)
		}
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

func maybeRaiseInterruptibleBid(database *sql.DB, client cloud.Client, ci *db.Launch, inst *cloud.Instance) bool {
	if database == nil || client == nil || ci == nil || inst == nil {
		return false
	}
	if ci.InstanceType != cloud.InstanceTypeInterruptible || ci.OnDemandRefCents == nil || *ci.OnDemandRefCents <= 0 {
		return false
	}
	if !isPausedProviderStatus(inst.Status) {
		return false
	}
	providerID := ci.EffectiveProviderID()
	if providerID == "" {
		return false
	}
	currentBidCents := ci.CostPerHourCents
	if ci.MaxBidPriceCents != nil {
		currentBidCents = *ci.MaxBidPriceCents
	}
	targetCents := *ci.OnDemandRefCents
	if targetCents <= currentBidCents {
		return false
	}
	bidder, ok := client.(cloud.BidClient)
	if !ok {
		return false
	}
	targetPrice := float64(targetCents) / 100.0
	if err := bidder.ChangeBid(providerID, targetPrice); err != nil {
		slog.Warn("failed to raise interruptible bid",
			"component", "reconcile", "instance", ci.ID, "provider_instance", providerID,
			"from_cents", currentBidCents, "to_cents", targetCents, "error", err)
		return false
	}
	if err := db.UpdateLaunchMaxBidPriceCents(database, ci.ID, targetCents); err != nil {
		slog.Warn("failed to record raised interruptible bid",
			"component", "reconcile", "instance", ci.ID, "provider_instance", providerID,
			"to_cents", targetCents, "error", err)
		return false
	}
	slog.Info("raised interruptible bid after provider pause",
		"component", "reconcile", "instance", ci.ID, "provider_instance", providerID,
		"from_cents", currentBidCents, "to_cents", targetCents)
	return true
}

func populateRunningPhaseTerminalJob(params *CheckInstanceParams, jobs []*db.Job, outcomes map[int64]string) {
	if params == nil {
		return
	}
	verb, phaseJobID, ok := ParsePhaseJobID(params.InstancePhase)
	if !ok || verb != PhaseRunning || phaseJobID <= 0 {
		return
	}
	for _, job := range jobs {
		if job == nil || job.ID != phaseJobID {
			continue
		}
		status := AttemptDisplayStatus(job, outcomes)
		if !IsJobTerminal(status) || job.EndTime == nil || *job.EndTime <= 0 {
			return
		}
		end := time.Unix(*job.EndTime, 0)
		params.RunningPhaseJobID = phaseJobID
		params.RunningPhaseJobStatus = status
		params.RunningPhaseJobTerminalSince = &end
		return
	}
}

// syncJobCompletionsFromR2 checks R2 for per-job .complete markers and records
// any completions in the DB. This ensures that jobs which finished successfully
// are in terminal status before ExecuteAction resets or closes attempts.
func syncJobCompletionsFromR2(database *sql.DB, r2Client *r2.Client, instanceID int64) {
	jobs, _ := db.GetLaunchJobsIncludingAttempts(database, instanceID)
	attemptOutcomes, _ := db.GetAttemptOutcomesByLaunch(database, instanceID)
	ctx := context.Background()
	var synced, remaining int
	for _, j := range jobs {
		status := AttemptDisplayStatus(j, attemptOutcomes)
		if !IsJobTerminal(status) {
			if reconcileCheckAndSyncJobComplete(ctx, r2Client, database, j.ID) {
				synced++
			} else {
				remaining++
			}
			continue
		}
		if j.LatestRunID != nil && (j.ExitCode == nil || j.StartTime == 0 || j.EndTime == nil || *j.EndTime == 0) {
			if reconcileCheckAndSyncJobCompleteRun(ctx, r2Client, database, j.ID, *j.LatestRunID) {
				synced++
			}
		}
	}
	if synced > 0 || remaining > 0 {
		slog.Debug("synced job completions from R2",
			"component", "reconcile", "instance", instanceID,
			"synced", synced, "remaining_non_terminal", remaining)
	}
}

// refreshBootstrapSurvival repopulates the per-provider bootstrap threshold
// cache when it has expired, and is a no-op within survivalCacheTTL of the last
// refresh. A provider whose survival computation fails is left out of the map;
// callers treat an absent entry as "no thresholds", not as zero thresholds.
func (r *Reconciler) refreshBootstrapSurvival(database *sql.DB, providers map[string]bool) {
	r.mu.Lock()
	expired := time.Since(r.bootstrapTimeoutsAt) >= survivalCacheTTL
	r.mu.Unlock()
	if !expired {
		return
	}

	// Compute outside the lock; ComputeBootstrapSurvival queries the DB once
	// per provider and other reconciler state stays available meanwhile.
	fresh := make(map[string]*db.BootstrapSurvival, len(providers))
	for provider := range providers {
		survival, err := db.ComputeBootstrapSurvival(database, provider)
		if err != nil {
			slog.Warn("failed to compute bootstrap survival", "component", "reconcile", "provider", provider, "error", err)
			continue
		}
		fresh[provider] = survival
		slog.Debug("bootstrap thresholds computed", "component", "reconcile", "provider", provider, "warn", survival.Warn, "terminate", survival.Terminate, "sample_size", survival.SampleSize)
	}

	r.mu.Lock()
	r.bootstrapTimeouts = fresh
	r.bootstrapTimeoutsAt = time.Now()
	r.mu.Unlock()
}

// bootstrapSurvivalFor returns the cached thresholds for a provider, or nil
// when none were computed. Read under the lock: reconcileOneInstance runs one
// goroutine per instance, and a concurrent reconcile pass may be refreshing.
func (r *Reconciler) bootstrapSurvivalFor(provider string) *db.BootstrapSurvival {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bootstrapTimeouts[provider]
}

// getSetupSurvival returns cached setup survival thresholds for a command+workdir,
// recomputing when the cache has expired.
// setupSurvivalForPhase resolves the learned setup thresholds for the job the
// instance phase names, or nil when the phase names no setup job. Shared by
// both CheckParams callers: the watcher lost its setup thresholds once
// already, and a second copy of this derivation is how it would happen again —
// the caller-parity guard compares fields assigned after CheckParams, not the
// expressions feeding it.
func (r *Reconciler) setupSurvivalForPhase(database *sql.DB, instancePhase string, jobs []*db.Job) *db.SetupSurvival {
	verb, phaseJobID, ok := ParsePhaseJobID(instancePhase)
	if !ok || verb != PhaseSetup {
		return nil
	}
	j := findJobInSlice(jobs, phaseJobID)
	if j == nil {
		return nil
	}
	return r.getSetupSurvival(database, j.Command, j.WorkingDir)
}

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

// resultsVerifyVerdict combines the manifest's two verification signals into a
// verified flag and, when not verified, a reason code for display. An
// incomplete upload is the more actionable signal, so it takes precedence over
// a coverage gap when both are present.
func resultsVerifyVerdict(uploadsOK, covers bool) (verified bool, detail string) {
	switch {
	case uploadsOK && covers:
		return true, ""
	case !uploadsOK:
		return false, db.ResultsVerifyDetailUploadsIncomplete
	default:
		return false, db.ResultsVerifyDetailManifestMissingJobs
	}
}

// verifyInstanceResults reads the R2 completion manifest for a completed instance
// and sets the results_verified flag based on upload statuses.
func verifyInstanceResults(database *sql.DB, r2Client *r2.Client, instanceID int64) {
	manifest := readR2CompletionManifest(r2Client, instanceID)
	if manifest == nil {
		// Legacy marker or missing — leave results_verified as NULL
		return
	}
	uploadsOK := manifest.AllUploadsOK()
	covers := completionManifestCoversLaunchJobs(database, instanceID, manifest)
	verified, detail := resultsVerifyVerdict(uploadsOK, covers)
	if err := db.UpdateLaunchResultsVerified(database, instanceID, verified, detail); err != nil {
		slog.Warn("failed to update results_verified", "component", "reconcile", "instance", instanceID, "error", err)
		return
	}
	if !verified {
		slog.Warn("instance completed but results unverified", "component", "reconcile", "instance", instanceID, "detail", detail)
	}
}

func completionManifestCoversLaunchJobs(database *sql.DB, instanceID int64, manifest *runner.InstanceCompletionManifest) bool {
	if database == nil || manifest == nil {
		return false
	}
	seen := make(map[int64]struct{}, len(manifest.Jobs))
	for _, job := range manifest.Jobs {
		seen[job.JobID] = struct{}{}
	}

	// Expected jobs are those dispatched to this launch — keyed on launch_id,
	// which is written when the job is routed to the instance. Do not filter on
	// cloud_outcome: at instance-termination time the per-job completion may not
	// yet be credited from R2 (the attempt can still read "queued"), which would
	// make this query return zero rows and spuriously fail verification even
	// though the manifest reports every upload OK.
	rows, err := database.Query(
		`SELECT DISTINCT job_id
		   FROM job_attempts
		  WHERE launch_id = ?
		    AND job_id IS NOT NULL`,
		instanceID,
	)
	if err != nil {
		slog.Warn("failed to verify completion manifest coverage", "component", "reconcile", "instance", instanceID, "error", err)
		return false
	}
	defer rows.Close()

	required := 0
	for rows.Next() {
		var jobID int64
		if err := rows.Scan(&jobID); err != nil {
			slog.Warn("failed to scan launch job for completion manifest coverage", "component", "reconcile", "instance", instanceID, "error", err)
			return false
		}
		required++
		if _, ok := seen[jobID]; !ok {
			return false
		}
	}
	if err := rows.Err(); err != nil {
		slog.Warn("failed to verify completion manifest coverage", "component", "reconcile", "instance", instanceID, "error", err)
		return false
	}
	return required > 0
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
	marker.State = instanceintent.StateSucceeded
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

// noteProviderStatusPoll updates the provider-status-unknown tracking for a
// launch and returns how long its provider status has been continuously
// unknown. unknown means the most recent poll yielded no instance data and a
// non-nil error (the CheckInstanceParams ProviderInst/ProviderErr contract).
// Any pass where status is known — a poll that returned instance data, or no
// poll at all — clears the tracking, so the returned duration measures an
// unbroken stretch of status-less polls (mirrors the firstDeadAt /
// probeFailures hysteresis maps).
func (r *Reconciler) noteProviderStatusPoll(id int64, unknown bool, now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !unknown {
		delete(r.firstUnknownAt, id)
		return 0
	}
	if r.firstUnknownAt == nil {
		r.firstUnknownAt = make(map[int64]time.Time)
	}
	first, ok := r.firstUnknownAt[id]
	if !ok {
		r.firstUnknownAt[id] = now
		return 0
	}
	return now.Sub(first)
}

func (r *Reconciler) clearProbeFailure(id int64) {
	r.mu.Lock()
	delete(r.probeFailures, id)
	r.mu.Unlock()
}

func (r *Reconciler) noteProbeFailure(id int64, now time.Time) probeFailureState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.probeFailures == nil {
		r.probeFailures = make(map[int64]probeFailureState)
	}
	state := r.probeFailures[id]
	if state.Count == 0 {
		state.FirstAt = now
	}
	state.Count++
	r.probeFailures[id] = state
	return state
}

// checkStaleHeartbeat evaluates the reconciler-specific stale-heartbeat
// watchdog: heartbeat stale past threshold, plus an SSH probe reporting the
// agent gone (positive evidence) or unreachable past both hysteresis
// minimums (see HeartbeatStale in specs/campaign-lifecycle.allium).
// Returns ActionNone while the launch is healthy or the evidence is
// insufficient. The caller executes the action through
// executeReconcileAction so this watchdog shares the completion-sync,
// destroy-before-mark, oplog, and disk-report pipeline; the caller clears
// the probe-failure state only once the action actually executes, so a
// deferred destroy retries without restarting the hysteresis window.
func (r *Reconciler) checkStaleHeartbeat(r2Client *r2.Client, ci *db.Launch, inst *cloud.Instance, now time.Time) InstanceAction {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	heartbeat, heartbeatAge := fetchReconcileHeartbeat(ctx, r2Client, ci.ID)
	if heartbeat != nil && heartbeat.observationUnknown {
		return InstanceAction{Kind: ActionNone}
	}
	staleThreshold := effectiveHeartbeatStaleThreshold(ci.AgentReadyAtUnix, now)
	if heartbeatAge == 0 || heartbeatAge <= staleThreshold {
		r.clearProbeFailure(ci.ID)
		return InstanceAction{Kind: ActionNone}
	}

	var agentAlive bool
	var err error
	if inst == nil {
		err = errors.New("provider status unknown; no current SSH endpoint")
	} else {
		agentAlive, err = probeCampaignAgent(inst, 15*time.Second)
	}
	if err == nil && agentAlive {
		r.clearProbeFailure(ci.ID)
		return InstanceAction{Kind: ActionNone}
	}

	if err != nil {
		// A probe error is the unknown case; a probe that reaches the host
		// and reports the agent gone is positive evidence, handled above.
		state := r.noteProbeFailure(ci.ID, now)
		elapsed := now.Sub(state.FirstAt)
		if state.Count < minProbeFailureAttempts || elapsed < minProbeFailureWindow {
			slog.Debug("heartbeat stale and agent probe unreachable, waiting before termination", "component", "reconcile", "instance", ci.ID, "heartbeat_age", heartbeatAge.Truncate(time.Second), "attempt", state.Count, "min_attempts", minProbeFailureAttempts, "elapsed", elapsed.Truncate(time.Second), "min_window", minProbeFailureWindow)
			return InstanceAction{Kind: ActionNone}
		}
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
	return InstanceAction{
		Kind:              ActionStaleHeartbeat,
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: reason,
		StallMessage:      fmt.Sprintf("heartbeat stale %s, reason=%s", heartbeatAge.Truncate(time.Second), reason),
		DestroyProvider:   true,
		ResetJobs:         true,
		AttemptOutcome:    db.AttemptOutcomeOrphaned,
	}
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
					slog.Debug("campaign had no instances, marking failed", "component", "reconcile", "campaign", c.ID, "status", db.CampaignStatusFailed)
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

		// A terminal instance can leave behind a job that was requeued for
		// relaunch. Don't end the campaign while any job that ran on its
		// instances is still non-terminal: the autopilot will relaunch it
		// into this campaign, and ending now would strand that relaunch on
		// an already-terminal campaign.
		if !allCampaignJobsTerminal(database, instances) {
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
			slog.Debug("campaign transitioned to terminal state", "component", "reconcile", "campaign", c.ID, "status", status)
			c.Status = status
			completed = append(completed, c)
		}
	}
	return completed, nil
}

// allCampaignJobsTerminal reports whether every job that ran on any of
// the given launches has reached a terminal job-effective status. Uses
// job_status (the canonical per-job view) so a historical attempt's
// orphaned/canceled/superseded status — which the launch_job_membership
// view can echo as 'queued' — doesn't block a campaign whose jobs have
// since completed elsewhere.
func allCampaignJobsTerminal(database *sql.DB, instances []*db.Launch) bool {
	jobIDs := make(map[int64]struct{})
	for _, inst := range instances {
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, inst.ID)
		if err != nil {
			slog.Warn("reconcile campaigns: load launch jobs",
				"component", "reconcile", "instance", inst.ID, "error", err)
			return false
		}
		for _, j := range jobs {
			jobIDs[j.ID] = struct{}{}
		}
	}
	if len(jobIDs) == 0 {
		return true
	}
	ids := make([]int64, 0, len(jobIDs))
	for id := range jobIDs {
		ids = append(ids, id)
	}
	jobs, err := db.GetJobsByIDs(database, ids)
	if err != nil {
		slog.Warn("reconcile campaigns: read job_status",
			"component", "reconcile", "error", err)
		return false
	}
	// Missing rows (tombstoned) keep the campaign open — surfacing the
	// divergence is safer than silently terminating.
	if len(jobs) < len(jobIDs) {
		slog.Warn("reconcile campaigns: job_status missing some campaign jobs",
			"component", "reconcile", "want", len(jobIDs), "got", len(jobs))
		return false
	}
	for _, j := range jobs {
		if !db.IsTerminalStatus(j.EffectiveStatus()) {
			return false
		}
	}
	return true
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
	data, found, err := fetchR2Marker(ctx, r2Client, key)
	if err != nil || !found || data == "" {
		return false
	}

	return applyGraceStatusMarker(data, ci, database)
}

// applyGraceStatusMarker parses a grace status marker and, when the agent is
// actually idle in grace (state "waiting"), transitions the DB instance to
// grace with the marker's deadline. The marker persists across the whole
// grace-wait session: while the agent runs resubmitted jobs it rewrites the
// marker with state "running", and the deadline in that marker may be stale
// (pre-extension — see cmd/agent/gracewait.go). Treating a "running" (or any
// non-"waiting") marker as grace would stamp an already-expired deadline and
// let the expiry check force-destroy a working instance.
func applyGraceStatusMarker(data string, ci *db.Launch, database *sql.DB) bool {
	var payload graceStatusPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		slog.Warn("failed to parse grace status", "component", "reconcile", "instance", ci.ID, "error", err)
		return false
	}

	if payload.State != controlplane.GraceStateWaiting {
		slog.Debug("grace status marker not in waiting state; skipping grace transition",
			"component", "reconcile", "instance", ci.ID, "state", payload.State)
		return false
	}

	if payload.Deadline.Unix == 0 {
		return false
	}

	slog.Debug("instance entered grace-wait", "component", "reconcile", "instance", ci.ID, "deadline", payload.Deadline.Unix)
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
	data, found, err := fetchR2Marker(ctx, r2Client, r2keys.CampaignComplete(instanceID))
	if err != nil || !found || data == "" {
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

	data, found, fetchErr := fetchR2Marker(ctx, r2Client, r2keys.InstanceTerminationIntent(instanceID))
	if fetchErr != nil || !found || data == "" {
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

// maxEmptyStatusTime is the legacy fallback maximum time to wait for a provider
// instance to report a non-empty status. It is used only when bootstrap survival
// data is unavailable. In normal operation instance_check.go uses the bootstrap
// survival terminate threshold instead.
const maxEmptyStatusTime = 1 * time.Minute

// maxEmptyStatusTimeCeiling bounds how far learned bootstrap survival data may
// stretch the empty-status window.
//
// BootstrapSurvival is measured from provider_running_at to the first job's
// wrapper start, so it describes work that begins only once the provider
// reports `running`. This rule waits for the provider to report any status at
// all, which is strictly earlier — the two intervals do not overlap, and the
// curve carries no information about this one. Unclamped it stretched a
// one-minute window past an hour and a half.
//
// Ceiling derived from the launched_at→provider_running_at distribution
// (n=4270 as of 2026-08-18): p90 ≈ 5.7 min, p99 ≈ 23 min. Reaching a non-empty
// status is a strict prefix of reaching `running`, so that distribution bounds
// this one from above; 10 minutes clears its p90 with margin while keeping the
// window an order of magnitude below the borrowed curve.
const maxEmptyStatusTimeCeiling = 10 * time.Minute

// maxPreRunningStatusTime is the legacy fallback maximum time to wait for a
// provider instance to reach "running" status. It is used only when bootstrap
// survival data is unavailable. In normal operation instance_check.go uses the
// bootstrap survival terminate threshold instead.
const maxPreRunningStatusTime = 5 * time.Minute

// maxPreRunningStatusTimeCeiling bounds how far learned bootstrap survival
// data may stretch the stale-non-running-status window.
//
// Same disjointness as maxEmptyStatusTimeCeiling: this rule waits for the
// provider to reach `running`, and the borrowed curve starts measuring there.
//
// The ceiling is looser than the empty-status one because the event is
// genuinely slower — provisioning and image pull happen in this window — and
// because the 5-minute constant is demonstrably too tight, which is why the
// learned override was adopted in the first place. Against the
// launched_at→provider_running_at distribution (n=4270 as of 2026-08-18),
// 5 min sits near p88: it would reap 12.5% of vast.ai launches that went on to
// run. 30 minutes sits above p99 (23 min vast.ai, 26 min RunPod). Relative to
// the unclamped learned value it newly reaps ~0.4% of launches (24 vs 9 of
// 3864 vast.ai launches ran later than 30 and 110 minutes respectively) and
// reclaims roughly 80 minutes of rental on each wedged one.
const maxPreRunningStatusTimeCeiling = 30 * time.Minute

// maxProviderStatusUnavailableTime is the maximum time to wait when provider
// status polling fails or omits a known launch before any job/phase progress is
// visible. Use the broader launch safety-net deadline because this path has no
// positive provider status signal; short provider/API gaps should not fail new
// launches faster than the normal bootstrap/launching watchdogs.
const maxProviderStatusUnavailableTime = launchingPhaseTimeout

// stalePauseTimeout is the maximum time an interruptible instance may sit in
// provider "stopped" state before we give up waiting for the provider to
// resume it and relaunch the jobs on a fresh offer. Chosen generously so a
// short outbid window does not churn launches.
const stalePauseTimeout = 6 * time.Hour

// idleAfterReadyTimeout is the maximum time an instance may sit in `running`
// state with the agent reporting "ready" but never starting a job. The
// bootstrap-stall watchdog stops applying once bootstrap_stage flips to
// "ready" (see instance_check.go rule 5); without this check, an agent that
// reaches ready but fails to drain the queue (R2 read failure, sidecar-only
// liveness, internal hang) creates a watchdog dead-zone where the heartbeat
// keeps the instance "alive" indefinitely while doing zero work. The agent's
// only post-ready job is to pick up queued work, so this window can be
// short.
const idleAfterReadyTimeout = 15 * time.Minute

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
	return isProviderTerminalWithPolicy(inst, false)
}

// providerConfirmedGone reports whether there is positive provider evidence
// that an *expected* destroy has landed: either an observed destroyed/dead
// status, or a lookup that failed with the
// confirmed-absence sentinel (cloud.ErrInstanceNotFound). It backs the
// termination-intent post-processing, where the agent already declared
// it is shutting down and not-found corroborates that the instance is
// gone. It is NOT a license to initiate termination: rule 7
// (provider-dead) requires an observed terminal instance, because
// vast.ai's endpoints transiently report not-found for still-booting
// instances (see TestReconcileLaunches_FallbackInstanceNotFound_DoesNotMarkDead).
// A nil instance with any other error — or with no error at all,
// meaning the launch was never polled this pass (no provider ID
// recorded yet because the create call is still in flight, or no
// client configured for its provider) — is unknown, not terminal.
func providerConfirmedGone(inst *cloud.Instance, providerErr error) bool {
	if inst != nil {
		return !needsProviderDestroy(inst)
	}
	return errors.Is(providerErr, cloud.ErrInstanceNotFound)
}

func isProviderTerminalWithPolicy(inst *cloud.Instance, pauseTolerant bool) bool {
	if inst == nil {
		// No observed instance: nothing to probe or destroy. This is NOT
		// confirmed death — callers needing positive evidence of absence
		// use providerConfirmedTerminal, and rule 7 requires a non-nil
		// instance before calling here.
		return true
	}
	switch inst.Status {
	case cloud.ProviderStatusExited, cloud.ProviderStatusDestroyed, cloud.ProviderStatusError, cloud.ProviderStatusDead:
		return true
	}
	if isPausedProviderStatus(inst.Status) {
		return !pauseTolerant
	}
	// Provider intended to stop/destroy but Status hasn't caught up yet
	// (e.g., Status still "created" while IntendedStatus is "stopped")
	if (inst.IntendedStatus == cloud.ProviderStatusStopped || inst.IntendedStatus == cloud.ProviderStatusDestroyed) &&
		inst.Status != cloud.ProviderStatusRunning {
		if pauseTolerant && inst.IntendedStatus == cloud.ProviderStatusStopped {
			return false
		}
		return true
	}
	return false
}

// isPausedProviderStatus reports whether the provider status indicates a
// pause that may resume — Vast.ai's "stopped" (explicit pause / credit
// hold) and "offline" (interruptible preemption with data preserved) are
// both treated this way. Used by isProviderTerminalWithPolicy and rule
// 4a-pause to decide whether to wait for resume.
func isPausedProviderStatus(status string) bool {
	return status == cloud.ProviderStatusStopped || status == cloud.ProviderStatusOffline
}

func isRecoverablePausedProviderStatus(status string, pauseTolerant bool) bool {
	switch status {
	case cloud.ProviderStatusStopped:
		return true
	case cloud.ProviderStatusOffline:
		return pauseTolerant
	default:
		return false
	}
}

func hasPreemptibleJobs(jobs []*db.Job) bool {
	for _, job := range jobs {
		if job != nil && job.UsesPreemptiblePlacement() {
			return true
		}
	}
	return false
}
