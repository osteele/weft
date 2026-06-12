package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/prestage"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2resolve"
	"github.com/osteele/weft/internal/remote"
	"github.com/osteele/weft/internal/ssh"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/util"
	"github.com/osteele/weft/internal/workdir"
)

const minHFInputStageTimeout = 10 * time.Minute

// dispatchEventDedupeWindow is how long a queue.dispatch.{failed,deferred}
// row suppresses an identical follow-up. Two AP-side processes hammering
// the same recurring failure (e.g., SIGKILL'd rsync every 30s) otherwise
// fill lifecycle_events with byte-identical rows; the hydrator already
// collapses them in the display path, but the audit log gets noisy. The
// window must be larger than the typical sync tick (60s) so consecutive
// retries dedupe, and small enough that a fresh recurrence after a
// genuine cleared interval still records.
const dispatchEventDedupeWindow = 5 * time.Minute

// sourceSyncLeaseTTL is longer than the source rsync process timeout. It keeps
// overlapping daemon passes from spawning identical rsyncs when the host-sync
// wrapper has already moved on but the previous sync cleanup is still unwinding.
const sourceSyncLeaseTTL = 2 * time.Minute

// defaultSourceSyncTimeout is the source-sync rsync budget used when a caller
// leaves HostSyncOptions.SourceTimeout unset. Source sync rsyncs the working
// tree over a freshly established SSH connection, so it needs far more than the
// few-second SSH status-probe budget carried by HostSyncOptions.Timeout. A
// status probe timing out is cheap and retried next tick; a source rsync
// SIGKILL'd mid-connect leaves the job blocked indefinitely. Bounded well under
// the SyncModeFull host lease (15m).
const defaultSourceSyncTimeout = 2 * time.Minute

// effectiveSourceSyncTimeout returns the budget for source-tree rsync. Unlike
// the SSH status timeout, it never falls back to HostSyncOptions.Timeout: an
// unset SourceTimeout uses defaultSourceSyncTimeout so a slow-to-connect host
// cannot wedge a queued job behind a connect-time SIGKILL. Callers that need a
// tighter or looser budget set SourceTimeout explicitly.
func effectiveSourceSyncTimeout(opts HostSyncOptions) time.Duration {
	if opts.SourceTimeout > 0 {
		return opts.SourceTimeout
	}
	return defaultSourceSyncTimeout
}

// HostSyncOptions configures a full host sync.
type HostSyncOptions struct {
	Timeout       time.Duration
	SourceTimeout time.Duration
	SkipSamples   bool
	NoQueueStart  bool
	// Mode controls which sync work to perform. Zero value uses full behavior.
	Mode SyncMode
	// UseBatchSync uses batched SSH calls for queue-runner jobs (faster for many jobs).
	UseBatchSync bool
	// Logger for sync messages. If nil, uses slog.Default().
	// Use NewQuietSyncLogger() to suppress SSH connection errors.
	Logger *slog.Logger
}

// SyncMode controls which portions of host sync execute.
type SyncMode string

const (
	SyncModeFull   SyncMode = "full"
	SyncModeStatus SyncMode = "status"
	SyncModeRepair SyncMode = "repair"
)

func (o HostSyncOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

func (o HostSyncOptions) mode() SyncMode {
	if o.Mode == "" {
		return SyncModeFull
	}
	return o.Mode
}

// HostSyncResult contains the outcome of syncing all jobs on a host.
type HostSyncResult struct {
	Updated            int
	HostContacted      bool
	QueueStarted       bool
	QueueDispatchError string
	QueueRunnerError   string
}

// EnsureQueueRunnerFunc is the function signature for starting queue runners.
// Returns (started, error).
type EnsureQueueRunnerFunc func(host string) (bool, error)

// SyncHost performs a full sync of all jobs on a host.
// It syncs active jobs, reconciles pending operations, checks for restarted jobs,
// syncs draft jobs, and optionally ensures the queue runner is started.
func SyncHost(database *sql.DB, host string, opts HostSyncOptions, ensureQueueRunner EnsureQueueRunnerFunc) (HostSyncResult, error) {
	var result HostSyncResult
	syncOpts := SyncOptions{Timeout: opts.Timeout, SkipSamples: opts.SkipSamples}
	syncLog := opts.logger()
	mode := opts.mode()
	seenJobs := make(map[int64]bool)
	timeout := effectiveSyncTimeout(opts.Timeout)
	sourceTimeout := effectiveSourceSyncTimeout(opts)

	queueOpsResult, err := ProcessDeferredQueueOps(database, host, timeout)
	if err != nil {
		return result, err
	}
	if queueOpsResult.HostContacted {
		result.HostContacted = true
	}

	var activeJobs []*db.Job
	if mode != SyncModeRepair {
		// Step 1: Sync active jobs
		activeJobs, err = db.ListActiveJobs(database, host)
		if err != nil {
			return result, err
		}

		if opts.UseBatchSync {
			// Batch mode: separate queue-runner and tmux jobs.
			// Jobs with PendingStatus need reconciliation, not batch sync,
			// so they go through SyncAndReconcile to apply pending operations.
			var queueRunnerJobs, tmuxJobs []*db.Job
			for _, job := range activeJobs {
				seenJobs[job.ID] = true
				if job.PendingStatus != nil {
					res, err := SyncAndReconcile(database, job, ReconcileOptions{Timeout: syncOpts.Timeout})
					if err != nil {
						syncLog.Debug("failed to reconcile job", "job_id", job.ID, "host", host, "error", err)
						continue
					}
					if res != nil {
						result.HostContacted = true
						result.Updated++
					}
				} else if job.UsesQueueRunner() {
					queueRunnerJobs = append(queueRunnerJobs, job)
				} else {
					tmuxJobs = append(tmuxJobs, job)
				}
			}
			if len(queueRunnerJobs) > 0 {
				updatedCount, err := BatchSyncQueueRunnerJobs(database, host, queueRunnerJobs, timeout)
				if err != nil {
					syncLog.Debug("batch sync failed", "host", host, "error", err)
				} else {
					result.Updated += updatedCount
					result.HostContacted = true
				}
				// Don't fail entire sync for batch error
			}
			for _, job := range tmuxJobs {
				syncResult, err := SyncJob(database, job, syncOpts)
				if err != nil {
					syncLog.Debug("failed to sync job", "job_id", job.ID, "host", host, "error", err)
					continue
				}
				if syncResult.HostContacted {
					result.HostContacted = true
				}
				if syncResult.Updated {
					result.Updated++
				}
			}
		} else {
			// Full mode: sync each job individually, using SyncAndReconcile for pending ops
			consecutiveFailures := 0
			for _, job := range activeJobs {
				seenJobs[job.ID] = true
				if job.PendingStatus != nil {
					_, err := SyncAndReconcile(database, job, ReconcileOptions{Timeout: syncOpts.Timeout})
					if err != nil {
						syncLog.Debug("failed to reconcile job", "job_id", job.ID, "host", host, "error", err)
						continue
					}
					result.HostContacted = true
					result.Updated++
					consecutiveFailures = 0
				} else {
					syncResult, err := SyncJob(database, job, syncOpts)
					if err != nil {
						return result, err
					}
					if syncResult.HostContacted {
						result.HostContacted = true
						consecutiveFailures = 0
					} else {
						consecutiveFailures++
						// If we've never contacted the host and have consecutive failures,
						// the host is likely unreachable — bail out early
						if !result.HostContacted && consecutiveFailures >= 2 {
							return result, nil
						}
					}
					if syncResult.Updated {
						result.Updated++
					}
				}
			}
		}

		// Ensure locally-queued jobs exist on the remote queue.
		// This catches jobs that were recorded locally while the host was
		// unreachable, or that the batch status path couldn't detect.
		// Runs regardless of HostContacted so brand-new jobs with no other
		// active jobs on the host can still be pushed.
		if mode == SyncModeFull {
			ensured, contacted, err := ensureQueuedJobsOnRemote(database, host, timeout, sourceTimeout, syncLog)
			result.Updated += ensured
			if contacted {
				result.HostContacted = true
			}
			if err != nil {
				if !ssh.IsConnectionError(err.Error()) {
					result.QueueDispatchError = err.Error()
					syncLog.Debug("failed to dispatch queued jobs", "host", host, "error", err)
				}
			}
		}

		// Step 2: Reconcile jobs with pending operations (kill, cancel, draft→queued, etc.)
		// This runs before the host-contacted guard so that draft→queued transitions
		// (which have no active jobs) can contact the host and proceed.
		pendingJobs, err := db.ListJobsPendingReconciliation(database, host)
		if err != nil {
			return result, err
		}
		for _, job := range pendingJobs {
			if seenJobs[job.ID] {
				continue
			}
			seenJobs[job.ID] = true
			res, err := SyncAndReconcile(database, job, ReconcileOptions{Timeout: syncOpts.Timeout})
			if err != nil {
				syncLog.Debug("failed to reconcile pending job", "job_id", job.ID, "host", host, "error", err)
				continue
			}
			if res != nil {
				result.HostContacted = true
				result.Updated++
			}
		}
	}

	// If the host was never contacted in steps 1-2, skip remaining SSH-dependent steps.
	// This avoids spending minutes timing out on an unreachable host.
	if !result.HostContacted {
		return result, nil
	}

	if mode != SyncModeStatus {
		// Step 3: Check for restarted jobs (failed/dead queue-runner jobs that may be running again)
		lastCheck := db.GetLastRestartCheck(database, host)
		checkTime := time.Now()

		restartedJobs, err := db.ListPotentiallyRestartedJobs(database, host)
		if err != nil {
			return result, err
		}

		// Optimization: filter to jobs with recent activity if we have a last-check time
		if !lastCheck.IsZero() && len(restartedJobs) > 0 {
			sshHost := remote.NewSSHHost(host, timeout)
			recentJobIDs, err := sshHost.GetRecentlyModifiedJobIDs(lastCheck)
			if err == nil && recentJobIDs != nil {
				recentSet := make(map[int64]bool)
				for _, id := range recentJobIDs {
					recentSet[id] = true
				}
				var filtered []*db.Job
				for _, job := range restartedJobs {
					if recentSet[job.ID] {
						filtered = append(filtered, job)
					}
				}
				restartedJobs = filtered
			}
		}

		if opts.UseBatchSync {
			// Batch mode for restarted jobs
			var restartedQueueJobs []*db.Job
			for _, job := range restartedJobs {
				if seenJobs[job.ID] {
					continue
				}
				seenJobs[job.ID] = true
				restartedQueueJobs = append(restartedQueueJobs, job)
			}
			if len(restartedQueueJobs) > 0 {
				updatedCount, err := BatchSyncQueueRunnerJobs(database, host, restartedQueueJobs, timeout)
				if err != nil {
					syncLog.Debug("batch sync of restarted jobs failed", "host", host, "error", err)
				} else {
					result.Updated += updatedCount
					result.HostContacted = true
				}
			}
		} else {
			for _, job := range restartedJobs {
				if seenJobs[job.ID] {
					continue
				}
				seenJobs[job.ID] = true
				syncResult, err := SyncJob(database, job, syncOpts)
				if err != nil {
					syncLog.Debug("failed to sync restarted job", "job_id", job.ID, "host", host, "error", err)
					continue
				}
				if syncResult.HostContacted {
					result.HostContacted = true
				}
				if syncResult.Updated {
					result.Updated++
				}
			}
		}

		// Update last restart check time only if we contacted the host
		if result.HostContacted {
			if err := db.UpdateLastRestartCheck(database, host, checkTime); err != nil {
				syncLog.Debug("failed to update restart check time", "host", host, "error", err)
			}
		}

		// Step 4: Sync draft jobs
		if mode == SyncModeFull {
			drafts, err := db.ListDraftJobsPendingSync(database, host)
			if err != nil {
				return result, err
			}
			for _, job := range drafts {
				if seenJobs[job.ID] {
					continue
				}
				syncResult, err := SyncDraftJob(database, job, syncOpts)
				if err != nil {
					syncLog.Debug("failed to sync draft job", "job_id", job.ID, "host", host, "error", err)
					continue
				}
				if syncResult.HostContacted {
					result.HostContacted = true
				}
				if syncResult.Updated {
					result.Updated++
				}
			}
		}
		// Step 5: Ensure queue runner is started
		if mode == SyncModeFull && !opts.NoQueueStart && ensureQueueRunner != nil {
			queueRunnerCount := 0
			for _, job := range activeJobs {
				if job.UsesQueueRunner() && (job.Status == db.StatusQueued || job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused) {
					queueRunnerCount++
				}
			}
			if queueRunnerCount > 0 {
				started, err := ensureQueueRunner(host)
				if err != nil {
					if !ssh.IsConnectionError(err.Error()) {
						result.QueueRunnerError = err.Error()
					}
				} else {
					result.HostContacted = true
					if started {
						result.QueueStarted = true
					}
				}
			}
		}
	}

	// Step 6: Record host sync time and scan HF caches
	if result.HostContacted {
		if err := db.RecordHostSync(database, host, time.Now()); err != nil {
			syncLog.Debug("failed to record host sync time", "host", host, "error", err)
		}
		if mode == SyncModeFull {
			scanHFCacheDuringSync(database, host, timeout)
		}
	}

	return result, nil
}

// fetchRemoteRunnerState reads the runner's state.json from the remote
// host. The wrapping shell `if [ -e ] then cat else echo SENTINEL`
// distinguishes a not-yet-started runner from an empty SSH response. The
// former maps to (nil, nil); the latter is an error so callers do not
// redispatch every synced job as missing from runner state.
func fetchRemoteRunnerState(host string, timeout time.Duration) (*opsqueue.RunnerState, error) {
	const noFileSentinel = "__WEFT_NO_STATE_FILE__"
	cmd := fmt.Sprintf(`if [ -e %s ]; then cat %s; else echo %s; fi`,
		opsqueue.StateFilePath(), opsqueue.StateFilePath(), noFileSentinel)
	stdout, _, err := ssh.TryRunWithTimeout(host, cmd, timeout)
	if err != nil {
		return nil, err
	}
	stdout = strings.TrimSpace(stdout)
	if stdout == noFileSentinel {
		return nil, nil
	}
	if stdout == "" {
		return nil, fmt.Errorf("empty runner state response (transient read failure?)")
	}
	var state opsqueue.RunnerState
	if err := json.Unmarshal([]byte(stdout), &state); err != nil {
		return nil, fmt.Errorf("parse runner state: %w", err)
	}
	return &state, nil
}

// pruneStaleRunnerPendings sends cancel ops for entries in state.Pending
// whose DB row says they shouldn't be there: terminal status, retargeted
// to a different host, or absent from the DB entirely. This is the reverse
// half of host_sync reconciliation — the forward half (DB → runner) ensures
// queued work reaches the runner; the reverse half (runner → DB) sweeps the
// runner's local queue of entries the DB has moved past.
//
// Without this, the agent's pending list grows unboundedly over time. A
// queued job that ran, completed, and was post-processed leaves a stale
// entry in `state.Pending` because the runner never received a `cancel` op
// for it — there was no need at the time it transitioned. New arrivals get
// buried behind the accumulated stale entries; the TUI reads the DB and
// shows them at "head of queue" while the agent has them at position N+.
//
// We deliberately leave entries alone when:
//   - The DB row is missing (could be in-flight insertion racing with sync)
//   - The DB row says queued-on-this-host (the runner is right to have it)
//   - The job is currently running or transient-state per its DB attempt
//
// The check is best-effort: a missed prune costs one extra dispatch tick of
// the agent skipping a stale entry. A wrong prune (canceling a job that
// should still run) is more expensive, so we err on the side of leaving
// ambiguous entries in place.
func pruneStaleRunnerPendings(database *sql.DB, host string, state *opsqueue.RunnerState, jobs, syncedJobs []*db.Job, timeout time.Duration, syncLog *slog.Logger) {
	if state == nil || len(state.Pending) == 0 {
		return
	}
	// Build the set of IDs we expect the runner to legitimately hold.
	expected := make(map[int64]struct{}, len(jobs)+len(syncedJobs))
	for _, j := range jobs {
		if j != nil {
			expected[j.ID] = struct{}{}
		}
	}
	for _, j := range syncedJobs {
		if j != nil {
			expected[j.ID] = struct{}{}
		}
	}

	pruned := 0
	for _, pendingID := range state.Pending {
		if _, ok := expected[pendingID]; ok {
			continue
		}
		job, err := db.GetJobByID(database, pendingID)
		if err != nil {
			syncLog.Debug("prune: lookup failed; leaving entry", "host", host, "job_id", pendingID, "error", err)
			continue
		}
		shouldPrune, reason := shouldPruneStalePending(job, host)
		if !shouldPrune {
			continue
		}
		if err := removeFromQueueFile(host, pendingID, timeout); err != nil {
			syncLog.Debug("prune: cancel op failed; will retry next sync", "host", host, "job_id", pendingID, "error", err)
			continue
		}
		pruned++
		syncLog.Debug("prune: removed stale runner pending entry", "host", host, "job_id", pendingID, "reason", reason)
	}
	if pruned > 0 {
		syncLog.Info("pruned stale runner pending entries", "host", host, "count", pruned)
	}
}

// shouldPruneStalePending classifies whether a runner-pending entry that
// isn't in our expected set should be removed via a cancel op. Returns the
// reason as a short tag for logging.
func shouldPruneStalePending(job *db.Job, host string) (bool, string) {
	if job == nil {
		// No DB row at all — the job was deleted or never tracked. Safe to
		// prune; the runner would fail to start it anyway (no job file).
		return true, "db_row_missing"
	}
	es := job.EffectiveStatus()
	switch es {
	case db.StatusCompleted, db.StatusFailed, db.StatusDead, db.StatusKilled, db.StatusCanceled:
		// Terminal in the DB's view; the runner has no business
		// trying to start it.
		return true, "db_terminal:" + es
	case db.StatusQueued:
		// DB says queued, but the host target may have moved off this
		// runner. Three cases prune:
		//   - Retargeted to a different inventory host.
		//   - Retargeted to a rental instance (TargetKind becomes
		//     RentalInstance, so HasInventoryHost is false).
		//   - Moved to unplaced (TargetKind becomes Unplaced).
		// Only "still inventory-targeted at THIS host" should keep the
		// runner entry. Positively identifying that case is safer than
		// enumerating the negatives, especially since new target kinds
		// added later would otherwise default to "leave alone" and
		// leak entries again.
		if job.HasInventoryHost() && job.Host == host {
			// Runner is right to have it. (This branch shouldn't fire if
			// the expected set is correct, but defensively skip prune.)
			return false, "db_queued_here"
		}
		return true, "db_queued_on_other_host"
	default:
		// Running, starting, paused, etc. — transient, leave it.
		return false, "db_transient:" + es
	}
}

// isJobInRunnerState reports whether a job ID is present in the runner's live state
// (pending queue, current job, running map, or finished map). A nil state is
// treated as empty.
//
// Finished is included because the laptop's last_synced_status lags the
// runner's terminal state: a job that finished on the runner may still be
// `queued` in the local DB for a short reconciliation window, and re-dispatch
// during that window was previously a no-op (the runner's JobCompleted gate
// silently skipped the add). The archive-on-add fix turned that no-op into a
// real re-run; checking Finished here restores the intended invariant —
// reconcile the DB to completed, do not re-dispatch.
func isJobInRunnerState(jobID int64, state *opsqueue.RunnerState) bool {
	if state == nil {
		return false
	}
	if state.Current != nil && *state.Current == jobID {
		return true
	}
	if slices.Contains(state.Pending, jobID) {
		return true
	}
	jobIDStr := strconv.FormatInt(jobID, 10)
	if _, ok := state.Running[jobIDStr]; ok {
		return true
	}
	_, ok := state.Finished[jobIDStr]
	return ok
}

type remoteJobPayload struct {
	exists bool
	runID  int64
}

func fetchRemoteJobPayloads(host string, jobs []*db.Job, timeout time.Duration) (map[int64]remoteJobPayload, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(jobs))
	seen := make(map[int64]struct{}, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if _, ok := seen[job.ID]; ok {
			continue
		}
		seen[job.ID] = struct{}{}
		ids = append(ids, strconv.FormatInt(job.ID, 10))
	}
	if len(ids) == 0 {
		return nil, nil
	}

	cmd := fmt.Sprintf(`for id in %s; do f=%s/job-${id}.json; if [ -e "$f" ]; then run=$(jq -r '.run_id // 0' "$f" 2>/dev/null || echo INVALID); printf '%%s	%%s\n' "$id" "$run"; else printf '%%s	MISSING\n' "$id"; fi; done`,
		strings.Join(ids, " "), opsqueue.QueueDir)
	stdout, _, err := ssh.TryRunWithTimeout(host, cmd, timeout)
	if err != nil {
		return nil, err
	}

	payloads := make(map[int64]remoteJobPayload, len(ids))
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("parse remote job payload probe line %q", line)
		}
		jobID, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse remote job payload id %q: %w", parts[0], err)
		}
		if parts[1] == "MISSING" {
			payloads[jobID] = remoteJobPayload{}
			continue
		}
		runID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			payloads[jobID] = remoteJobPayload{exists: true, runID: -1}
			continue
		}
		payloads[jobID] = remoteJobPayload{exists: true, runID: runID}
	}
	return payloads, nil
}

func queuedJobMaterializedInRunner(job *db.Job, state *opsqueue.RunnerState, payloads map[int64]remoteJobPayload) bool {
	if job == nil || state == nil {
		return false
	}
	jobIDStr := strconv.FormatInt(job.ID, 10)
	if _, ok := state.Finished[jobIDStr]; ok {
		return true
	}
	if running, ok := state.Running[jobIDStr]; ok {
		return runIDsCompatible(job.LatestRunID, running.RunID)
	}
	if state.Current != nil && *state.Current == job.ID {
		return true
	}
	if !slices.Contains(state.Pending, job.ID) {
		return false
	}
	payload, ok := payloads[job.ID]
	if !ok || !payload.exists {
		return false
	}
	return runIDsCompatible(job.LatestRunID, payload.runID)
}

func runIDsCompatible(want *int64, got int64) bool {
	return want == nil || *want == 0 || got == 0 || *want == got
}

// ensureQueuedJobsOnRemote pushes locally-queued jobs to the remote host.
// It fetches its own job list via ListUnsyncedQueuedJobs, resolves the backend
// once per host, syncs sources (deduplicated by working directory), and appends
// jobs to the remote queue (or submits via sbatch for Slurm hosts).
// Returns (ensured count, host contacted, error).
func ensureQueuedJobsOnRemote(database *sql.DB, host string, timeout, sourceTimeout time.Duration, syncLog *slog.Logger) (int, bool, error) {
	jobs, err := db.ListUnsyncedQueuedJobs(database, host)
	if err != nil {
		return 0, false, err
	}

	// Check jobs that were previously dispatched (last_synced_status='queued') but
	// may be missing from the runner's live state (runner crashed, glibc mismatch, etc.).
	// These are safe to re-dispatch because AppendJobToQueue / AddPending is idempotent.
	syncedJobs, err := db.ListSyncedQueuedJobs(database, host)
	if err != nil {
		return 0, false, err
	}

	// Fetch the runner state once and reconcile in BOTH directions:
	//   - Forward: DB-queued jobs missing from runner state → re-dispatch.
	//   - Reverse: runner-pending jobs that the DB no longer considers
	//     queued-on-this-host → send cancel ops to prune.
	//
	// The reverse direction is what keeps the agent's local queue from
	// accumulating completed-but-never-canceled entries indefinitely. Old
	// jobs that ran and finished but were never explicitly canceled stay in
	// the agent's `pending` array forever; new arrivals get buried behind
	// them; the TUI (which reads DB-as-truth) shows the new job at the head
	// of the queue while the agent has it at position N+. Reverse-reconcile
	// converges the two views.
	state, stateErr := fetchRemoteRunnerState(host, timeout)
	if stateErr != nil {
		syncLog.Debug("could not read runner state", "host", host, "error", stateErr)
	}

	// Forward reconcile: re-dispatch syncedJobs that the runner doesn't know
	// about. We gate on stateErr (not state) so the no-state-file case —
	// runner not yet started, intentional (nil, nil) sentinel — still
	// triggers re-dispatch: isJobInRunnerState(_, nil) returns false, so
	// every syncedJob is treated as missing and reposted. AppendJobToQueue
	// is idempotent, so a queue file that already holds the entry is
	// unchanged. Only a genuine state-read failure (stateErr != nil) leaves
	// the synced set untouched.
	var payloads map[int64]remoteJobPayload
	if stateErr == nil && len(syncedJobs) > 0 {
		var payloadErr error
		payloads, payloadErr = fetchRemoteJobPayloads(host, syncedJobs, timeout)
		if payloadErr != nil {
			syncLog.Debug("could not read runner job payloads", "host", host, "error", payloadErr)
		}
		for _, job := range syncedJobs {
			if payloadErr == nil && queuedJobMaterializedInRunner(job, state, payloads) {
				continue // runner has it — nothing to do
			}
			// Runner doesn't know about this job. Reset so it gets re-dispatched below.
			syncLog.Debug("job missing from runner state, re-dispatching", "job_id", job.ID, "host", host)
			if err := db.ResetLastSyncedStatus(database, job.ID); err != nil {
				syncLog.Debug("failed to reset last_synced_status", "job_id", job.ID, "error", err)
				continue
			}
			jobs = append(jobs, job)
		}
	}

	// Reverse reconcile requires the actual pending list, so it stays gated
	// on a non-nil state.
	if state != nil {
		pruneStaleRunnerPendings(database, host, state, jobs, syncedJobs, timeout, syncLog)
	}

	var r2Client *r2.Client
	var r2ClientErr error
	r2ClientLoaded := false
	getR2Client := func() (*r2.Client, error) {
		if r2ClientLoaded {
			return r2Client, r2ClientErr
		}
		r2ClientLoaded = true
		r2Client, r2ClientErr = defaultR2Client()
		if r2ClientErr != nil {
			r2ClientErr = fmt.Errorf("create R2 client for cloud dependencies: %w", r2ClientErr)
			return nil, r2ClientErr
		}
		return r2Client, nil
	}

	var failures []string
	recordFailure := func(jobID int64, stage string, err error) {
		failures = append(failures, fmt.Sprintf("job %s %s: %v", ids.FormatJobID(jobID), stage, err))
		_, _ = db.InsertLifecycleEventDedup(database, &db.LifecycleEvent{
			EventKind: db.EventQueueDispatchFailed,
			JobID:     jobID,
			Detail:    truncateDispatchDetail(stage + ": " + err.Error()),
		}, dispatchEventDedupeWindow)
	}

	// Stage rental-produced --needs artifacts for every queued job on this host
	// — synced or not — so jobs already sitting in the runner's pending list can
	// have their satisfied markers written without having to be re-dispatched.
	// One SSH probe covers every job's marker/file state; staging then runs
	// only for needs whose marker is missing.
	allQueued := make([]*db.Job, 0, len(syncedJobs)+len(jobs))
	seenStage := make(map[int64]bool)
	for _, job := range append(append([]*db.Job(nil), syncedJobs...), jobs...) {
		if job == nil || seenStage[job.ID] {
			continue
		}
		seenStage[job.ID] = true
		allQueued = append(allQueued, job)
	}
	stageFailures := stageArtifactNeedsForHost(database, host, allQueued, timeout, getR2Client)
	for jobID, err := range stageFailures {
		syncLog.Debug("artifact needs staging failed", "job_id", jobID, "host", host, "error", err)
		recordFailure(jobID, "artifact needs staging failed", err)
		oplog.LogJob(oplog.OpJobStartFailed, jobID, host,
			oplog.WithDetail("artifact needs staging failed"),
			oplog.WithError(err),
		)
	}

	if len(jobs) == 0 {
		return 0, false, nil
	}

	// Resolve backend once for this host (all jobs share the same backend).
	backend := jobs[0].Backend
	contacted := false
	if backend == "" {
		var err error
		backend, err = ResolveBackend(host, timeout)
		if err != nil {
			if ssh.IsConnectionError(err.Error()) {
				return 0, false, nil
			}
			return 0, false, err
		}
		contacted = true
	}

	ensured := 0
	syncedDirs := make(map[string]bool)
	sourceSyncFailedDirs := make(map[string]error)
	sourceSyncDeferredDirs := make(map[string]string)
	sourceSHAByDir := make(map[string]string)
	recordDispatchOK := func(jobID int64) {
		recordQueueDispatchOK(database, jobID)
	}
	recordDeferred := func(jobID int64, stage string, err error) {
		detail := stage
		if err != nil {
			detail = stage + ": " + err.Error()
		}
		_, _ = db.InsertLifecycleEventDedup(database, &db.LifecycleEvent{
			EventKind: db.EventQueueDispatchDeferred,
			JobID:     jobID,
			Detail:    truncateDispatchDetail(detail),
		}, dispatchEventDedupeWindow)
	}
	for _, job := range jobs {
		if _, failed := stageFailures[job.ID]; failed {
			continue
		}
		ready, reason, err := cloudDepsReady(database, job)
		if err != nil {
			recordFailure(job.ID, "dependency check failed", err)
			continue
		}
		if !ready {
			syncLog.Debug("cloud dependency not ready; deferring dispatch", "job_id", job.ID, "host", host, "reason", reason)
			continue
		}

		// Set backend if not yet stored
		if job.Backend == "" {
			if err := db.SetJobBackend(database, job.ID, backend); err != nil {
				return ensured, contacted, err
			}
			job.Backend = backend
		}

		// Layer D: R2-isolated source fallback. When a job has previously
		// failed the runner's preflight provenance check (per-job marker
		// from Layer C didn't hold — e.g. because rsync --delete reaped it
		// before our exclude pattern shipped, or because the working dir
		// got nuked), escalate to a content-addressed R2 tarball so the
		// runner can extract byte-identical sources into a per-job dir,
		// bypassing the shared working directory entirely.
		//
		// Two escalation triggers:
		//   (1) source_provenance_mismatch dispatch event — emitted by
		//       the new agent's PREFLIGHT_REJECTED protocol. Has an
		//       expectedSHA encoded in the detail, so we can do the
		//       pin_lost vs. matching-SHA check.
		//   (2) any prior failed attempt — covers legacy pre-fix runners
		//       that recorded provenance failures only via the runner log
		//       and a non-zero exit, with no lifecycle event. No
		//       expectedSHA is recoverable, so we upload the current
		//       laptop SHA and accept that the job may run against newer
		//       sources than originally queued. The alternative is the
		//       job sitting queued forever, which is what happens to
		//       jobs that failed under the pre-92be8b4f1 runner.
		useR2Source := false
		sourceR2Key := ""
		escalateToR2 := func(reasonTag string) {
			localDir := workdir.ResolveLocal(job.WorkingDir)
			r2c, r2Err := getR2Client()
			if r2Err != nil {
				syncLog.Debug("R2 client unavailable; falling back to normal sync", "job_id", job.ID, "error", r2Err)
				return
			}
			uploadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			key, uploadErr := srcsync.UploadSourceToR2ForInputs(uploadCtx, r2c, localDir, job.Inputs)
			cancel()
			if uploadErr != nil {
				syncLog.Debug("R2 source upload failed; falling back to normal sync", "job_id", job.ID, "error", uploadErr)
				return
			}
			useR2Source = true
			sourceR2Key = key
			syncLog.Info("escalated to R2-isolated source mode",
				"job_id", job.ID, "host", host, "r2_key", key, "reason", reasonTag)
		}
		if backend != db.BackendSlurm && job.WorkingDir != "" {
			provFails, perr := db.CountJobDispatchFailuresMatching(database, job.ID, "source_provenance_mismatch")
			if perr != nil {
				syncLog.Debug("count prior provenance failures failed", "job_id", job.ID, "error", perr)
			}
			failedAttempts, faerr := db.CountJobFailedAttempts(database, job.ID)
			if faerr != nil {
				syncLog.Debug("count prior failed attempts failed", "job_id", job.ID, "error", faerr)
			}

			// The prov_fails path is preferred when available because it can
			// do the pin_lost vs. matching-SHA check, which produces a
			// richer diagnosis than the legacy fallback. But if its inner
			// branches can't act (expectedSHA missing from the event
			// detail, or local ComputeSourceSHA256 errors), we still want
			// to fall through to legacy escalation when there's evidence
			// of a prior failure — silently wedging is worse than
			// escalating broadly.
			if provFails >= 1 {
				detail, detailErr := db.LatestJobDispatchFailureDetail(database, job.ID, "source_provenance_mismatch")
				if detailErr != nil {
					syncLog.Debug("read prior provenance failure detail failed", "job_id", job.ID, "error", detailErr)
				}
				expectedSHA := extractExpectedSHA(detail)
				if expectedSHA == "" {
					syncLog.Debug("prior provenance failure detail missing expected SHA; falling through to legacy escalation", "job_id", job.ID, "detail", detail)
				} else {
					localDir := workdir.ResolveLocal(job.WorkingDir)
					currentSHA, hashErr := srcsync.ComputeSourceSHA256(localDir)
					switch {
					case hashErr != nil:
						syncLog.Debug("source SHA recompute failed; falling through to legacy escalation", "job_id", job.ID, "error", hashErr)
					case currentSHA != expectedSHA:
						// Pin lost: the laptop tree has moved past the SHA
						// this job was queued against. We cannot
						// reconstruct the original snapshot. Surface so
						// diagnose tells the user what's actually wrong
						// instead of looping silently.
						reason := fmt.Sprintf("provenance_pin_lost: queued at %s, laptop now %s", shortSHA(expectedSHA), shortSHA(currentSHA))
						recordFailure(job.ID, reason, fmt.Errorf("%s", reason))
						continue
					default:
						// Laptop SHA still matches — upload the original
						// tarball to R2 (content-addressed, idempotent)
						// and switch the queue entry to R2-isolated mode.
						escalateToR2("prior provenance failure")
					}
				}
			}

			// Legacy escalation: prior failed attempt with no
			// source_provenance_mismatch dispatch event we could use.
			// Most likely a pre-PREFLIGHT_REJECTED runner that reported
			// failures only via the runner log and a non-zero exit.
			// Skipped if prov_fails escalation already succeeded — no
			// point uploading twice.
			if !useR2Source && failedAttempts >= 1 {
				escalateToR2("legacy failed attempt")
			}
		}

		// Sync sources, deduplicated by remote path.
		// If sync fails, skip this job — don't queue it with stale code.
		// Skipped entirely when this job is in R2-isolated mode.
		if !useR2Source && job.WorkingDir != "" {
			localDir := workdir.ResolveLocal(job.WorkingDir)
			remoteDir := workdir.ToTildeRelative(job.WorkingDir)
			// Back off when this host's source sync has been timing out: a
			// wedged working-dir mount makes every rsync hang until SIGKILL,
			// so retrying each tick just piles up unkillable rsyncs. Skip the
			// attempt until the streak's backoff elapses.
			if remaining := sourceSyncBackoffRemaining(database, job.Host, time.Now()); remaining > 0 {
				reason := fmt.Sprintf("source sync backing off (%s unresponsive), retry in %s", job.Host, remaining.Round(time.Second))
				sourceSyncDeferredDirs[remoteDir] = reason
				recordDeferred(job.ID, reason, nil)
				continue
			}
			if err := sourceSyncFailedDirs[remoteDir]; err != nil {
				recordFailure(job.ID, "source sync failed", err)
				continue
			}
			if reason := sourceSyncDeferredDirs[remoteDir]; reason != "" {
				recordDeferred(job.ID, reason, fmt.Errorf("%s", reason))
				continue
			}
			if !syncedDirs[remoteDir] {
				sourceSHA256 := ""
				if hash, hashErr := srcsync.ComputeSourceSHA256(localDir); hashErr != nil {
					syncLog.Debug("source fingerprint unavailable", "job_id", job.ID, "working_dir", job.WorkingDir, "host", job.Host, "error", hashErr)
				} else {
					sourceSHA256 = hash
				}
				leaseScope := sourceSyncLeaseScope(job.Host, remoteDir)
				leaseOwner := sourceSyncLeaseOwner()
				acquired, leaseErr := db.AcquireAutoLease(database, leaseScope, leaseOwner, sourceSyncLeaseTTLFor(sourceTimeout))
				if leaseErr != nil {
					recordFailure(job.ID, "source sync lease failed", leaseErr)
					continue
				}
				if !acquired {
					reason := "source sync already in flight"
					sourceSyncDeferredDirs[remoteDir] = reason
					recordDeferred(job.ID, reason, nil)
					oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
						oplog.WithDetailf("%s: %s", reason, remoteDir),
					)
					continue
				}
				leaseReleased := false
				releaseLease := func() {
					if leaseReleased {
						return
					}
					leaseReleased = true
					_ = db.ReleaseAutoLease(database, leaseScope, leaseOwner)
				}
				if err := srcsync.SyncSourcesToHostWithTimeout(job.Host, localDir, remoteDir, job.Inputs, sourceTimeout); err != nil {
					releaseLease()
					if ssh.IsConnectionError(err.Error()) {
						// Host is offline — bail out silently, but
						// record a deferred event so the TUI sees a
						// fresh "waiting on host" reason instead of
						// the last hard failure.
						recordDeferred(job.ID, "source sync deferred (host unreachable)", err)
						return ensured, contacted, nil
					}
					syncLog.Debug("skipping job, source sync failed", "job_id", job.ID, "working_dir", job.WorkingDir, "host", job.Host, "error", err)
					oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host,
						oplog.WithDetail("source sync failed"),
						oplog.WithError(err),
					)
					sourceSyncFailedDirs[remoteDir] = err
					recordFailure(job.ID, "source sync failed", err)
					if errors.Is(err, srcsync.ErrSourceSyncTimeout) {
						// Wedge signal: rsync was SIGKILL'd on an
						// unresponsive dir. Extend the host's streak so
						// backoff and (past threshold) auto-cordon kick in.
						recordSourceSyncTimeout(database, job.Host)
					}
					continue
				}
				if err := srcsync.WriteRemoteSourceMarker(job.Host, remoteDir, sourceSHA256, timeout); err != nil {
					releaseLease()
					if ssh.IsConnectionError(err.Error()) {
						return ensured, contacted, nil
					}
					syncLog.Debug("source marker write failed", "job_id", job.ID, "working_dir", job.WorkingDir, "host", job.Host, "error", err)
				} else {
					releaseLease()
				}
				syncedDirs[remoteDir] = true
				sourceSHAByDir[remoteDir] = sourceSHA256
				// Recovery: a clean sync after a timeout streak resets the
				// streak and lifts any wedge auto-cordon. Only emit the ok
				// event when recovering so healthy syncs don't bloat the log.
				if cnt, _, streakErr := db.HostSourceSyncTimeoutStreak(database, job.Host); streakErr == nil && cnt > 0 {
					recordSourceSyncOK(database, job.Host, syncLog)
				}
			}
		}

		if job.Backend != db.BackendSlurm {
			if err := ensureHFInputsAvailable(database, job.Host, job.Inputs, timeout); err != nil {
				if ssh.IsConnectionError(err.Error()) {
					recordDeferred(job.ID, "input staging deferred (host unreachable)", err)
					return ensured, contacted, nil
				}
				syncLog.Debug("skipping job, HF input ensure failed", "job_id", job.ID, "host", job.Host, "error", err)
				oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host,
					oplog.WithDetail("input staging failed"),
					oplog.WithError(err),
				)
				recordFailure(job.ID, "input staging failed", err)
				continue
			}
		}
		// Route Slurm jobs to sbatch
		if job.Backend == db.BackendSlurm {
			outcome, err := requestJobStatus(database, job, db.StatusQueued, ExecuteOptions{Timeout: timeout})
			if err != nil {
				recordFailure(job.ID, "slurm submission failed", err)
				continue
			}
			if !outcome.hostAvailable {
				return ensured, contacted, nil
			}
			contacted = true
			if outcome.resolved {
				if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusQueued); err != nil {
					return ensured, contacted, err
				}
				if err := db.SetQueuedAtNow(database, job.ID); err != nil {
					return ensured, contacted, err
				}
				recordDispatchOK(job.ID)
				ensured++
			}
			continue
		}

		sourceSHA256 := ""
		if job.WorkingDir != "" {
			sourceSHA256 = sourceSHAByDir[workdir.ToTildeRelative(job.WorkingDir)]
		}
		// Stamp the per-job source marker. The runner reads this file (not
		// the rolling .weft-source.sha256) so peer-job syncs to the same
		// working dir won't invalidate this job's preflight provenance
		// check. Best-effort: a write failure here only loses the per-job
		// marker; the runner falls back to the rolling marker, which keeps
		// the legacy behaviour. Skipped in R2-isolated mode (the runner
		// reads from a per-job dir extracted from R2, not the shared dir).
		if !useR2Source && job.WorkingDir != "" && sourceSHA256 != "" {
			remoteDir := workdir.ToTildeRelative(job.WorkingDir)
			if err := srcsync.WriteRemoteSourceMarkerForJob(job.Host, remoteDir, job.ID, sourceSHA256, timeout); err != nil {
				if ssh.IsConnectionError(err.Error()) {
					return ensured, contacted, nil
				}
				syncLog.Debug("per-job source marker write failed", "job_id", job.ID, "working_dir", job.WorkingDir, "host", job.Host, "error", err)
			}
		}
		if err := AppendJobToQueueWithSourceAndR2(job, timeout, sourceSHA256, sourceR2Key); err != nil {
			if ssh.IsConnectionError(err.Error()) {
				recordDeferred(job.ID, "queue append deferred (host unreachable)", err)
				return ensured, contacted, nil
			}
			recordFailure(job.ID, "queue append failed", err)
			continue
		}
		contacted = true
		if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusQueued); err != nil {
			return ensured, contacted, err
		}
		if err := db.SetQueuedAtNow(database, job.ID); err != nil {
			return ensured, contacted, err
		}
		recordDispatchOK(job.ID)
		ensured++
	}
	// Past the wedge threshold this auto-cordons the host and unplaces its
	// queued inventory jobs so the autopilot re-places them on a healthy host.
	// Runs each pass that reached here (i.e. the host had queued work): cheap,
	// idempotent, and a no-op below threshold or once the host is auto-cordoned
	// and drained. Unconditional so a host already over threshold but currently
	// in backoff (no fresh attempt this pass) is still cordoned and re-placed.
	maybeHandleSourceSyncWedge(database, host, syncLog)
	if len(failures) > 0 {
		return ensured, contacted, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return ensured, contacted, nil
}

func defaultR2Client() (*r2.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config for R2: %w", err)
	}
	return r2.New(r2.Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	})
}

// truncateDispatchDetail caps a dispatch failure detail at a length suitable
// for storage and TUI display. rsync errors can be multi-line and very long;
// the first line is usually the actionable summary.
// findExistingScpToRemote returns the PIDs of running scp processes
// whose command line targets the given `<host>:<remotePath>`. Used as a
// belt-and-suspenders before spawning a new staging scp: the per-(host,
// artifact) DB lease catches the same-binary case, but a peer process
// running an older code version can bypass it. Match is intentionally
// narrow (host AND full remote path) to avoid false positives against
// unrelated scp's.
//
// pgrep's command-line match is portable across macOS and Linux. If
// pgrep is unavailable or fails, returns empty — the function falls
// open rather than blocking the staging path.
func findExistingScpToRemote(remoteHost, remotePath string) []int {
	if remoteHost == "" || remotePath == "" {
		return nil
	}
	needle := fmt.Sprintf("scp .*%s:%s", regexp.QuoteMeta(remoteHost), regexp.QuoteMeta(remotePath))
	out, err := exec.Command("pgrep", "-f", needle).Output()
	if err != nil {
		// pgrep returns non-zero when no matches found; that's the
		// happy path. Anything else (binary missing, permission) we
		// treat as "no opinion" and let the caller proceed.
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, perr := strconv.Atoi(line)
		if perr != nil {
			continue
		}
		// Don't report our own scp's-in-flight (none of which should
		// match the path of a not-yet-spawned scp anyway, but cheap
		// to filter).
		if pid == self {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

func truncateDispatchDetail(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return util.Truncate(strings.TrimSpace(s), 240)
}

// shortSHA returns the first 8 hex characters of a SHA, or the full string if
// shorter. Used in human-readable dispatch-failure reasons where the full
// 64-character hash would dominate the message.
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// extractExpectedSHA parses the "expected=<hex>" token out of a
// source_provenance_mismatch dispatch detail. Returns "" when the token is
// absent (older formats or truncated details).
func extractExpectedSHA(detail string) string {
	const prefix = "expected="
	i := strings.Index(detail, prefix)
	if i < 0 {
		return ""
	}
	rest := detail[i+len(prefix):]
	end := strings.IndexAny(rest, ", ")
	if end < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}

func cloudDepsReady(database *sql.DB, job *db.Job) (bool, string, error) {
	if job == nil {
		return true, "", nil
	}
	// See note in materializeCloudNeeds: refresh metadata to avoid acting on
	// a stale read that predates the submitter's metadata write.
	if fresh, err := db.GetJobByID(database, job.ID); err == nil && fresh != nil {
		job.Metadata = fresh.Metadata
	}
	if job.Metadata == nil || job.Metadata.Dependencies == nil {
		return true, "", nil
	}
	for _, dep := range job.Metadata.Dependencies.CloudAfter {
		if dep.JobID <= 0 {
			continue
		}
		upstream, err := db.GetJobByID(database, dep.JobID)
		if err != nil {
			return false, "", fmt.Errorf("lookup cloud dependency job %s: %w", ids.FormatJobID(dep.JobID), err)
		}
		if upstream == nil {
			return false, fmt.Sprintf("dependency job %s not found", ids.FormatJobID(dep.JobID)), nil
		}
		if !db.IsTerminalStatus(upstream.Status) {
			return false, fmt.Sprintf("dependency job %s not complete (%s)", ids.FormatJobID(dep.JobID), upstream.Status), nil
		}
		if !dep.AllowFailure && upstream.Status != db.StatusCompleted {
			return false, fmt.Sprintf("dependency job %s did not succeed (%s)", ids.FormatJobID(dep.JobID), upstream.Status), nil
		}
	}
	return true, "", nil
}

type pendingNeed struct {
	spec       string
	path       string
	producerID int64
	latestRun  *int64
	markerName string
	remotePath string
	// preResolvedR2Key is set for named-asset needs whose R2 key is known at
	// parse time (assets/<content_hash>). When set, stageMissingNeeds skips
	// r2resolve.NeedR2Key and uses this key directly.
	preResolvedR2Key string
}

// collectPendingNeeds parses one job's --needs and returns a pendingNeed for
// each rental-producer entry (on-prem producers are skipped — their satisfied
// marker is written by the producer's own queue runner). No SSH or R2 IO.
func collectPendingNeeds(database *sql.DB, job *db.Job) ([]pendingNeed, error) {
	if job == nil || len(job.Needs) == 0 || strings.TrimSpace(job.Host) == "" {
		return nil, nil
	}
	remoteBase := workdir.ToTildeRelative(job.WorkingDir)
	if remoteBase == "" {
		remoteBase = job.WorkingDir
	}
	var pending []pendingNeed
	for _, spec := range job.Needs {
		// Named-asset form: resolve to a pre-computed R2 key from named_assets.
		// Path comes from the asset's target_path recorded at publish time.
		if name, ok := parseAssetNeedSpec(spec); ok {
			asset, err := db.GetNamedAssetByName(database, name)
			if err != nil {
				return nil, fmt.Errorf("resolve --needs %q: %w", spec, err)
			}
			pending = append(pending, pendingNeed{
				spec:             spec,
				path:             asset.TargetPath,
				markerName:       filepath.Base(artifacts.NamedAssetSatisfiedFile("", name)),
				remotePath:       strings.TrimSuffix(remoteBase, "/") + "/" + strings.TrimPrefix(asset.TargetPath, "/"),
				preResolvedR2Key: r2keys.NamedAsset(asset.ContentHash),
			})
			continue
		}
		parsed, err := parseCloudNeedSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("parse --needs %q: %w", spec, err)
		}
		producer, err := db.GetJobByID(database, parsed.Version)
		if err != nil {
			return nil, fmt.Errorf("lookup producer job %s for %q: %w", ids.FormatJobID(parsed.Version), spec, err)
		}
		if producer == nil {
			return nil, fmt.Errorf("producer job %s for %q not found", ids.FormatJobID(parsed.Version), spec)
		}
		if producer.HasInventoryHost() {
			continue
		}
		pending = append(pending, pendingNeed{
			spec:       spec,
			path:       parsed.Path,
			producerID: producer.ID,
			latestRun:  producer.LatestRunID,
			markerName: fmt.Sprintf("artifact-%d-%s.satisfied", parsed.Version, url.PathEscape(parsed.Path)),
			remotePath: strings.TrimSuffix(remoteBase, "/") + "/" + strings.TrimPrefix(parsed.Path, "/"),
		})
	}
	return pending, nil
}

// parseAssetNeedSpec returns (name, true) if spec matches "asset:NAME".
func parseAssetNeedSpec(spec string) (string, bool) {
	const prefix = "asset:"
	if !strings.HasPrefix(spec, prefix) {
		return "", false
	}
	return spec[len(prefix):], true
}

// stageArtifactNeedsForHost ensures rental-produced --needs artifacts for
// every queued job on the host are present on the host: bytes are scp'd into
// the working dir and a satisfied marker is written under ~/.cache/weft/logs/
// so the queue runner's CheckDependencies passes. On-prem-producer needs are
// skipped (the producer's own queue runner writes the marker on completion).
// Idempotent — needs whose marker already exists are skipped.
//
// All jobs' marker/file presence is probed in a single SSH round-trip,
// regardless of how many jobs or specs there are; the per-job staging loop
// then reuses that state map. Each job's failures are reported via failed,
// without aborting the rest of the pass.
func stageArtifactNeedsForHost(database *sql.DB, host string, jobs []*db.Job, timeout time.Duration, getR2Client func() (*r2.Client, error)) (failed map[int64]error) {
	failed = make(map[int64]error)
	if strings.TrimSpace(host) == "" || len(jobs) == 0 {
		return
	}

	pendingByJob := make(map[int64][]pendingNeed)
	var allPending []pendingNeed
	for _, job := range jobs {
		pending, err := collectPendingNeeds(database, job)
		if err != nil {
			failed[job.ID] = err
			continue
		}
		if len(pending) == 0 {
			continue
		}
		pendingByJob[job.ID] = pending
		allPending = append(allPending, pending...)
	}
	if len(allPending) == 0 {
		return
	}

	state, err := probeRemoteNeedsStateFunc(host, allPending, timeout)
	if err != nil {
		err = fmt.Errorf("probe remote needs state: %w", err)
		for jobID := range pendingByJob {
			failed[jobID] = err
		}
		return
	}

	for _, job := range jobs {
		pending := pendingByJob[job.ID]
		if len(pending) == 0 {
			continue
		}
		var todo []pendingNeed
		for _, n := range pending {
			ent := state[n.markerName]
			if !ent.markerExists || ent.fileSize < 0 {
				todo = append(todo, n)
			}
		}
		if len(todo) == 0 {
			continue
		}
		if err := stageMissingNeeds(database, job, todo, state, timeout, getR2Client); err != nil {
			failed[job.ID] = err
		}
	}
	return
}

// needsStageLeaseTTL is the time the per-(host, artifact) DB lease is
// held during a staging transfer. Long enough that a 1 GB artifact at
// ~1 MB/s still fits, short enough that a wedged caller doesn't hold
// the lock forever.
const needsStageLeaseTTL = 15 * time.Minute

func needsStageLeaseScope(host, markerName string) string {
	return fmt.Sprintf("needs-stage:%s:%s", host, markerName)
}

func sourceSyncLeaseScope(host, remoteDir string) string {
	return fmt.Sprintf("source-sync:%s:%s", host, url.PathEscape(remoteDir))
}

func sourceSyncLeaseTTLFor(sourceTimeout time.Duration) time.Duration {
	leaseTTL := sourceSyncLeaseTTL
	if sourceTimeout > 0 && sourceTimeout+time.Minute > leaseTTL {
		leaseTTL = sourceTimeout + time.Minute
	}
	return leaseTTL
}

func sourceSyncLeaseOwner() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:pid=%d:%d", host, os.Getpid(), time.Now().UnixNano())
}

func needsStageLeaseOwner() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:pid=%d", host, os.Getpid())
}

// stageMissingNeeds executes the R2-download + scp + marker-write for the
// subset of needs whose satisfied marker is not yet on the host. `state`
// covers all of the job's needs and carries the file/staging sizes the probe
// already collected, so the size-match optimization (skip transfer when the
// on-host bytes already match R2) costs no extra SSH round-trips.
//
// Per-need DB lease (`needs-stage:<host>:<markerName>`) prevents duplicate
// concurrent transfers of the same artifact into the same host staging
// destination. The TTL is generous (15m) so slower transfers do not
// accidentally double-spawn after lease expiry.
func stageMissingNeeds(database *sql.DB, job *db.Job, todo []pendingNeed, state map[string]remoteNeedState, timeout time.Duration, getR2Client func() (*r2.Client, error)) error {
	// Lazy-init the R2 client and tmp dir: only pay the cost when at
	// least one need actually transfers (lease acquired + bytes
	// missing). All-skipped passes — every need locked by a peer
	// process, or every need already on-host — should be free.
	var (
		r2Client    *r2.Client
		tmpDir      string
		tmpDirErr   error
		ensureSetup = func() error {
			if r2Client != nil && tmpDir != "" {
				return nil
			}
			if tmpDirErr != nil {
				return tmpDirErr
			}
			c, err := getR2Client()
			if err != nil {
				return err
			}
			if c == nil || !c.IsConfigured() {
				return fmt.Errorf("R2 is not configured")
			}
			d, err := os.MkdirTemp("", fmt.Sprintf("weft-needs-%d-*", job.ID))
			if err != nil {
				tmpDirErr = fmt.Errorf("create temp dir for needs staging: %w", err)
				return tmpDirErr
			}
			r2Client, tmpDir = c, d
			return nil
		}
	)
	defer func() {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
		oplog.WithDetailf("artifact needs staging start (%d artifact%s)", len(todo), pluralize(len(todo))))
	for _, n := range todo {
		oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
			oplog.WithDetailf("artifact needs staging attempt: %s", n.spec))

		leaseScope := needsStageLeaseScope(job.Host, n.markerName)
		acquired, leaseErr := db.AcquireAutoLease(database, leaseScope, needsStageLeaseOwner(), needsStageLeaseTTL)
		if leaseErr != nil {
			return fmt.Errorf("%q: acquire staging lease: %w", n.spec, leaseErr)
		}
		if !acquired {
			oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
				oplog.WithDetailf("artifact needs staging skipped (in flight): %s", n.spec))
			_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
				EventKind: db.EventQueueDispatchDeferred,
				JobID:     job.ID,
				Detail:    truncateDispatchDetail("artifact staging in flight: " + n.spec),
			})
			continue
		}
		needLeaseReleased := false
		releaseLease := func() {
			if needLeaseReleased {
				return
			}
			needLeaseReleased = true
			_ = db.ReleaseAutoLease(database, leaseScope, needsStageLeaseOwner())
		}
		// Defer-and-loop: release on every exit path through the
		// per-need iteration. Inline error returns also release.

		// We have the lease and at least one byte of work to consider —
		// initialize the R2 client and tmp dir on first use.
		if err := ensureSetup(); err != nil {
			releaseLease()
			return fmt.Errorf("%q: %w", n.spec, err)
		}

		var key string
		if n.preResolvedR2Key != "" {
			key = n.preResolvedR2Key
		} else {
			resolved, err := r2resolve.NeedR2Key(context.Background(), r2Client, n.producerID, n.latestRun, n.path)
			if err != nil {
				releaseLease()
				return fmt.Errorf("%q: %w", n.spec, err)
			}
			key = resolved
		}
		stagingPath := n.remotePath + ".weft-staging"
		ent := state[n.markerName]

		// Pick the cheapest path that produces a complete file at the final
		// remote path. File presence alone is not proof of completeness — only
		// byte-count parity with R2 is. The two skip cases (already in place,
		// or staging-file complete) avoid a 500MB re-transfer when a prior
		// sync left bytes on disk.
		mvStaging := false
		needsTransfer := true
		if ent.fileSize >= 0 || ent.stagingSize >= 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			expected, err := r2Client.ObjectSize(ctx, key)
			cancel()
			if err != nil {
				releaseLease()
				return fmt.Errorf("get expected size for %q: %w", n.spec, err)
			}
			switch {
			case ent.fileSize == expected:
				needsTransfer = false
			case ent.stagingSize == expected:
				needsTransfer = false
				mvStaging = true
			}
		}

		if needsTransfer {
			// Belt-and-suspenders: even with the lease, an out-of-tree
			// caller (an old binary running concurrently, a manual
			// `weft job sync`, etc.) can bypass our DB lock. If an
			// existing scp is already pushing this exact artifact to
			// the same remote path, skip rather than stomp; concurrent
			// scp's to one destination produce torn writes and (under
			// macOS memory pressure) get OOM-killed.
			if pids := findExistingScpToRemote(job.Host, stagingPath); len(pids) > 0 {
				releaseLease()
				_, _ = db.InsertLifecycleEventDedup(database, &db.LifecycleEvent{
					EventKind: db.EventQueueDispatchDeferred,
					JobID:     job.ID,
					Detail:    truncateDispatchDetail(fmt.Sprintf("artifact staging skipped: peer scp(s) %v already targeting %s", pids, stagingPath)),
				}, dispatchEventDedupeWindow)
				oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
					oplog.WithDetailf("artifact needs staging skipped (peer scp in flight): %s", n.spec))
				continue
			}
			localPath := filepath.Join(tmpDir, filepath.FromSlash(strings.TrimPrefix(n.path, "/")))
			if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
				releaseLease()
				return fmt.Errorf("prepare local staging path for %q: %w", n.spec, err)
			}
			if _, err := r2Client.DownloadObjectToFileWithIdleTimeout(context.Background(), key, localPath, 5*time.Minute); err != nil {
				releaseLease()
				return fmt.Errorf("download %s for %q: %w", key, n.spec, err)
			}
			if err := ensureRemoteParentDir(job.Host, n.remotePath, timeout); err != nil {
				releaseLease()
				return fmt.Errorf("create remote dir for %q: %w", n.spec, err)
			}
			// scp to a sibling staging path then mv into place: guards against
			// a killed scp leaving a truncated file at the final path that a
			// future probe would mistake for a complete transfer.
			if err := ssh.CopyTo(localPath, job.Host, stagingPath); err != nil {
				releaseLease()
				return fmt.Errorf("copy %q to %s:%s: %w", n.spec, job.Host, stagingPath, err)
			}
			mvStaging = true
		}
		if err := finalizeRemoteNeed(job.Host, mvStaging, stagingPath, n.remotePath, n.markerName, timeout); err != nil {
			releaseLease()
			return fmt.Errorf("finalize %q: %w", n.spec, err)
		}
		releaseLease()
		oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
			oplog.WithDetailf("artifact needs staging success: %s", n.spec))
	}
	oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
		oplog.WithDetailf("artifact needs staging complete (%d artifact%s)", len(todo), pluralize(len(todo))))
	return nil
}

// finalizeRemoteNeed atomically renames the staging file (if requested) and
// writes the satisfied marker in a single SSH round-trip, halving the per-need
// SSH cost on the cold-transfer path.
func finalizeRemoteNeed(host string, mvStaging bool, stagingPath, remotePath, markerName string, timeout time.Duration) error {
	var sb strings.Builder
	if mvStaging {
		sb.WriteString(fmt.Sprintf("mv -f -- %s %s && ", shellQuote(stagingPath), shellQuote(remotePath)))
	}
	sb.WriteString(fmt.Sprintf("mkdir -p ~/.cache/weft/logs && printf '0\\n' > ~/.cache/weft/logs/%s", shellQuote(markerName)))
	if _, stderr, err := ssh.RunWithTimeout(host, sb.String(), timeout); err != nil {
		return fmt.Errorf("ssh finalize %s: %s: %w", markerName, stderr, err)
	}
	return nil
}

// remoteNeedState carries the marker presence + file/staging sizes captured
// by a single probe round-trip. Sizes are -1 when the path is missing.
type remoteNeedState struct {
	markerExists bool
	fileSize     int64
	stagingSize  int64
}

// probeRemoteNeedsStateFunc is the test seam for probeRemoteNeedsState. Tests
// override it to assert call count and shape without parsing shell.
var probeRemoteNeedsStateFunc = probeRemoteNeedsState

// probeRemoteNeedsState reports each need's marker presence and the byte
// sizes of its final-path file and `.weft-staging` sibling, in one SSH
// round-trip. Per spec the script emits exactly one line:
//
//	<markerName>\t<markerExists 0|1>\t<fileSize|-1>\t<stagingSize|-1>
//
// Reporting sizes here lets the size-match transfer-skip path skip a
// per-spec stat round-trip later.
func probeRemoteNeedsState(host string, needs []pendingNeed, timeout time.Duration) (map[string]remoteNeedState, error) {
	if len(needs) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString(`logs=~/.cache/weft/logs; statsz() { stat -f%z "$1" 2>/dev/null || stat -c%s "$1" 2>/dev/null || echo -1; }; `)
	for _, n := range needs {
		// eval so a leading "~/" in remotePath expands.
		sb.WriteString(fmt.Sprintf(
			`m=0; [ -e "$logs"/%s ] && m=1; fp=$(eval echo %s); fs=-1; [ -e "$fp" ] && fs=$(statsz "$fp"); sp=$(eval echo %s); ss=-1; [ -e "$sp" ] && ss=$(statsz "$sp"); printf '%%s\t%%s\t%%s\t%%s\n' %s "$m" "$fs" "$ss"; `,
			shellQuote(n.markerName),
			shellQuote(n.remotePath),
			shellQuote(n.remotePath+".weft-staging"),
			shellQuote(n.markerName),
		))
	}
	sb.WriteString(`exit 0`)
	out, _, err := ssh.RunWithTimeout(host, sb.String(), timeout)
	if err != nil {
		return nil, err
	}
	state := make(map[string]remoteNeedState)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) != 4 {
			continue
		}
		entry := remoteNeedState{fileSize: -1, stagingSize: -1}
		entry.markerExists = parts[1] == "1"
		if v, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
			entry.fileSize = v
		}
		if v, err := strconv.ParseInt(parts[3], 10, 64); err == nil {
			entry.stagingSize = v
		}
		state[parts[0]] = entry
	}
	return state, nil
}

type cloudNeedSpec struct {
	Path    string
	Version int64
}

func parseCloudNeedSpec(spec string) (cloudNeedSpec, error) {
	idx := strings.LastIndex(spec, ":")
	if idx < 0 {
		return cloudNeedSpec{}, fmt.Errorf("needs spec %q must include :version suffix", spec)
	}
	version, err := strconv.ParseInt(spec[idx+1:], 10, 64)
	if err != nil || version <= 0 {
		return cloudNeedSpec{}, fmt.Errorf("needs spec %q has invalid version", spec)
	}
	return cloudNeedSpec{Path: spec[:idx], Version: version}, nil
}

func ensureRemoteParentDir(host, remotePath string, timeout time.Duration) error {
	remoteDir := path.Dir(remotePath)
	if remoteDir == "." || remoteDir == "/" || remoteDir == "" {
		return nil
	}
	// Keep "~" expansion by running mkdir relative to $HOME for tilde paths.
	if strings.HasPrefix(remoteDir, "~/") {
		remoteDir = strings.TrimPrefix(remoteDir, "~/")
	}
	cmd := fmt.Sprintf("mkdir -p %s", shellQuote(remoteDir))
	_, stderr, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return fmt.Errorf("mkdir remote path: %s: %w", stderr, err)
	}
	return nil
}

func pluralize(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func ensureHFInputsAvailable(database *sql.DB, host string, inputs []string, timeout time.Duration) error {
	var hfInputs []dataloc.DataAsset
	for _, ref := range inputs {
		asset, ok := dataloc.ParseAssetRef(ref)
		if !ok {
			continue
		}
		if asset.Kind != dataloc.AssetHFModel && asset.Kind != dataloc.AssetHFDataset {
			continue
		}
		if !dataloc.IsHFModelID(asset.ID) {
			slog.Warn("skipping invalid HF input", "ref", ref, "host", host)
			continue
		}
		hfInputs = append(hfInputs, asset)
	}
	if len(hfInputs) == 0 {
		return nil
	}

	stageTimeout := hfInputStageTimeout(timeout)
	// Refresh target-host cache state first so we do not re-download assets that
	// are already present but missing from the local inventory DB.
	scanHFCacheDuringSync(database, host, stageTimeout)

	plan, err := prestage.BuildPlan(database, host, inventory.HostHFCacheDir(host), inputs)
	if err != nil {
		return err
	}
	if err := prestage.Execute(database, plan, stageTimeout); err != nil {
		return err
	}
	if len(plan.Transfers) > 0 {
		scanHFCacheDuringSync(database, host, stageTimeout)
	}

	for _, asset := range hfInputs {
		entries, err := dataloc.FindAssetHosts(database, asset)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(entries, func(e dataloc.HostDataEntry) bool {
			return e.Host == host
		}) {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), stageTimeout)
		entry, err := dataloc.DownloadAssetToHost(ctx, host, asset, "main")
		cancel()
		if err != nil {
			return err
		}
		entry.LastSeen = time.Now().UTC().Truncate(time.Second)
		if err := dataloc.RecordAsset(database, entry); err != nil {
			return err
		}
	}

	return nil
}

func hfInputStageTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout < minHFInputStageTimeout {
		return minHFInputStageTimeout
	}
	return timeout
}

// scanHFCacheDuringSync scans the remote HF cache and records discovered assets
// with their paths and sizes. Failures are silently ignored to avoid disrupting
// the sync flow.
func scanHFCacheDuringSync(database *sql.DB, host string, timeout time.Duration) {
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	entries, err := dataloc.ScanHFCacheDetailedContext(ctx, host)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		entry.LastSeen = now
		_ = dataloc.RecordAsset(database, entry)
	}
}
