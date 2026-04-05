package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/prestage"
	"github.com/osteele/weft/internal/remote"
	"github.com/osteele/weft/internal/ssh"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

const minHFInputStageTimeout = 10 * time.Minute

// HostSyncOptions configures a full host sync.
type HostSyncOptions struct {
	Timeout      time.Duration
	SkipSamples  bool
	NoQueueStart bool
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
			ensured, contacted, err := ensureQueuedJobsOnRemote(database, host, timeout, syncLog)
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
			scanHFCacheDuringSync(database, host)
		}
	}

	return result, nil
}

// fetchRemoteRunnerState reads the runner's state.json from the remote host.
// Returns nil (no error) if the file does not exist.
func fetchRemoteRunnerState(host string, timeout time.Duration) (*RunnerState, error) {
	stdout, _, err := ssh.TryRunWithTimeout(host, fmt.Sprintf("cat %s 2>/dev/null || true", StateFilePath()), timeout)
	if err != nil {
		return nil, err
	}
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, nil
	}
	var state RunnerState
	if err := json.Unmarshal([]byte(stdout), &state); err != nil {
		return nil, fmt.Errorf("parse runner state: %w", err)
	}
	return &state, nil
}

// isJobInRunnerState reports whether a job ID is present in the runner's live state
// (pending queue, current job, or running map). A nil state is treated as empty.
func isJobInRunnerState(jobID int64, state *RunnerState) bool {
	if state == nil {
		return false
	}
	if state.Current != nil && *state.Current == jobID {
		return true
	}
	if slices.Contains(state.Pending, jobID) {
		return true
	}
	_, ok := state.Running[strconv.FormatInt(jobID, 10)]
	return ok
}

// ensureQueuedJobsOnRemote pushes locally-queued jobs to the remote host.
// It fetches its own job list via ListUnsyncedQueuedJobs, resolves the backend
// once per host, syncs sources (deduplicated by working directory), and appends
// jobs to the remote queue (or submits via sbatch for Slurm hosts).
// Returns (ensured count, host contacted, error).
func ensureQueuedJobsOnRemote(database *sql.DB, host string, timeout time.Duration, syncLog *slog.Logger) (int, bool, error) {
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
	if len(syncedJobs) > 0 {
		state, stateErr := fetchRemoteRunnerState(host, timeout)
		if stateErr != nil {
			// Can't read state — skip re-dispatch check, we'll retry next sync cycle
			syncLog.Debug("could not read runner state", "host", host, "error", stateErr)
		} else {
			// state may be nil if the state file doesn't exist yet (runner not started)
			for _, job := range syncedJobs {
				if isJobInRunnerState(job.ID, state) {
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
	sourceSHAByDir := make(map[string]string)
	var failures []string
	recordFailure := func(jobID int64, stage string, err error) {
		failures = append(failures, fmt.Sprintf("job %d %s: %v", jobID, stage, err))
	}
	for _, job := range jobs {
		// Set backend if not yet stored
		if job.Backend == "" {
			if err := db.SetJobBackend(database, job.ID, backend); err != nil {
				return ensured, contacted, err
			}
			job.Backend = backend
		}

		// Sync sources, deduplicated by remote path.
		// If sync fails, skip this job — don't queue it with stale code.
		if job.WorkingDir != "" {
			localDir := workdir.ResolveLocal(job.WorkingDir)
			remoteDir := workdir.ToTildeRelative(job.WorkingDir)
			if !syncedDirs[remoteDir] {
				sourceSHA256 := ""
				if hash, hashErr := srcsync.ComputeSourceSHA256(localDir); hashErr != nil {
					syncLog.Debug("source fingerprint unavailable", "job_id", job.ID, "working_dir", job.WorkingDir, "host", job.Host, "error", hashErr)
				} else {
					sourceSHA256 = hash
				}
				if err := srcsync.SyncSourcesToHost(job.Host, localDir, remoteDir, job.Inputs); err != nil {
					if ssh.IsConnectionError(err.Error()) {
						// Host is offline — bail out silently
						return ensured, contacted, nil
					}
					syncLog.Debug("skipping job, source sync failed", "job_id", job.ID, "working_dir", job.WorkingDir, "host", job.Host, "error", err)
					oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host,
						oplog.WithDetail("source sync failed"),
						oplog.WithError(err),
					)
					syncedDirs[remoteDir] = true // don't retry same dir
					recordFailure(job.ID, "source sync failed", err)
					continue
				}
				if err := srcsync.WriteRemoteSourceMarker(job.Host, remoteDir, sourceSHA256, timeout); err != nil {
					if ssh.IsConnectionError(err.Error()) {
						return ensured, contacted, nil
					}
					syncLog.Debug("source marker write failed", "job_id", job.ID, "working_dir", job.WorkingDir, "host", job.Host, "error", err)
				}
				syncedDirs[remoteDir] = true
				sourceSHAByDir[remoteDir] = sourceSHA256
			}
		}

		if job.Backend != db.BackendSlurm {
			if err := ensureHFInputsAvailable(database, job.Host, job.Inputs, timeout); err != nil {
				if ssh.IsConnectionError(err.Error()) {
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
				if err := db.UpdateLastSyncedStatus(database, job.ID, db.StatusQueued); err != nil {
					return ensured, contacted, err
				}
				ensured++
			}
			continue
		}

		sourceSHA256 := ""
		if job.WorkingDir != "" {
			sourceSHA256 = sourceSHAByDir[workdir.ToTildeRelative(job.WorkingDir)]
		}
		if err := AppendJobToQueueWithSource(job, timeout, sourceSHA256); err != nil {
			if ssh.IsConnectionError(err.Error()) {
				return ensured, contacted, nil
			}
			recordFailure(job.ID, "queue append failed", err)
			continue
		}
		contacted = true
		if err := db.UpdateLastSyncedStatus(database, job.ID, db.StatusQueued); err != nil {
			return ensured, contacted, err
		}
		ensured++
	}
	if len(failures) > 0 {
		return ensured, contacted, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return ensured, contacted, nil
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

	// Refresh target-host cache state first so we do not re-download assets that
	// are already present but missing from the local inventory DB.
	scanHFCacheDuringSync(database, host)

	stageTimeout := hfInputStageTimeout(timeout)
	plan, err := prestage.BuildPlan(database, host, inventory.HostHFCacheDir(host), inputs)
	if err != nil {
		return err
	}
	if err := prestage.Execute(database, plan, stageTimeout); err != nil {
		return err
	}
	if len(plan.Transfers) > 0 {
		scanHFCacheDuringSync(database, host)
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
func scanHFCacheDuringSync(database *sql.DB, host string) {
	entries, err := dataloc.ScanHFCacheDetailed(host)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		entry.LastSeen = now
		_ = dataloc.RecordAsset(database, entry)
	}
}
