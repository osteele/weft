package ops

import (
	"context"
	"database/sql"
	"log"
	"slices"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/prestage"
	"github.com/osteele/weft/internal/remote"
	"github.com/osteele/weft/internal/ssh"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

// HostSyncOptions configures a full host sync.
type HostSyncOptions struct {
	Timeout      time.Duration
	SkipSamples  bool
	NoQueueStart bool
	// UseBatchSync uses batched SSH calls for queue-runner jobs (faster for many jobs).
	UseBatchSync bool
}

// HostSyncResult contains the outcome of syncing all jobs on a host.
type HostSyncResult struct {
	Updated          int
	HostContacted    bool
	QueueStarted     bool
	QueueRunnerError string
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
	seenJobs := make(map[int64]bool)
	timeout := effectiveSyncTimeout(opts.Timeout)

	queueOpsResult, err := ProcessDeferredQueueOps(database, host, timeout)
	if err != nil {
		return result, err
	}
	if queueOpsResult.HostContacted {
		result.HostContacted = true
	}

	// Step 1: Sync active jobs
	activeJobs, err := db.ListActiveJobs(database, host)
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
					log.Printf("sync: failed to reconcile job %d on %s: %v", job.ID, host, err)
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
				log.Printf("sync: batch sync failed for %s: %v", host, err)
			} else {
				result.Updated += updatedCount
				result.HostContacted = true
			}
			// Don't fail entire sync for batch error
		}
		for _, job := range tmuxJobs {
			syncResult, err := SyncJob(database, job, syncOpts)
			if err != nil {
				log.Printf("sync: failed to sync job %d on %s: %v", job.ID, host, err)
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
					log.Printf("sync: failed to reconcile job %d on %s: %v", job.ID, host, err)
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
	{
		ensured, contacted, err := ensureQueuedJobsOnRemote(database, host, timeout)
		if err == nil {
			result.Updated += ensured
			if contacted {
				result.HostContacted = true
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
			log.Printf("sync: failed to reconcile pending job %d on %s: %v", job.ID, host, err)
			continue
		}
		if res != nil {
			result.HostContacted = true
			result.Updated++
		}
	}

	// If the host was never contacted in steps 1-2, skip remaining SSH-dependent steps.
	// This avoids spending minutes timing out on an unreachable host.
	if !result.HostContacted {
		return result, nil
	}

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
				log.Printf("sync: batch sync of restarted jobs failed for %s: %v", host, err)
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
				log.Printf("sync: failed to sync restarted job %d on %s: %v", job.ID, host, err)
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
			log.Printf("sync: failed to update restart check time for %s: %v", host, err)
		}
	}

	// Step 4: Sync draft jobs
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
			log.Printf("sync: failed to sync draft job %d on %s: %v", job.ID, host, err)
			continue
		}
		if syncResult.HostContacted {
			result.HostContacted = true
		}
		if syncResult.Updated {
			result.Updated++
		}
	}

	// Step 5: Ensure queue runner is started
	if !opts.NoQueueStart && ensureQueueRunner != nil {
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

	// Step 6: Record host sync time and scan HF caches
	if result.HostContacted {
		if err := db.RecordHostSync(database, host, time.Now()); err != nil {
			log.Printf("sync: failed to record host sync time for %s: %v", host, err)
		}
		scanHFCacheDuringSync(database, host)
	}

	return result, nil
}

// ensureQueuedJobsOnRemote pushes locally-queued jobs to the remote host.
// It fetches its own job list via ListUnsyncedQueuedJobs, resolves the backend
// once per host, syncs sources (deduplicated by working directory), and appends
// jobs to the remote queue (or submits via sbatch for Slurm hosts).
// Returns (ensured count, host contacted, error).
func ensureQueuedJobsOnRemote(database *sql.DB, host string, timeout time.Duration) (int, bool, error) {
	jobs, err := db.ListUnsyncedQueuedJobs(database, host)
	if err != nil {
		return 0, false, err
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
	for _, job := range jobs {
		// Set backend if not yet stored
		if job.Backend == "" {
			if err := db.SetJobBackend(database, job.ID, backend); err != nil {
				return ensured, contacted, err
			}
			job.Backend = backend
		}

		// Sync sources, deduplicated by working directory.
		// If sync fails, skip this job — don't queue it with stale code.
		if job.WorkingDir != "" && !syncedDirs[job.WorkingDir] {
			localDir := workdir.ResolveLocal(job.WorkingDir)
			if err := srcsync.SyncSourcesToHost(job.Host, localDir, job.WorkingDir, job.Inputs); err != nil {
				log.Printf("sync: skipping job %d, source sync failed for %s on %s: %v", job.ID, job.WorkingDir, job.Host, err)
				syncedDirs[job.WorkingDir] = true // don't retry same dir
				continue
			}
			syncedDirs[job.WorkingDir] = true
		}

		if job.Backend != db.BackendSlurm {
			if err := ensureHFInputsAvailable(database, job.Host, job.Inputs, timeout); err != nil {
				log.Printf("sync: skipping job %d, HF input ensure failed on %s: %v", job.ID, job.Host, err)
				continue
			}
		}

		// Route Slurm jobs to sbatch
		if job.Backend == db.BackendSlurm {
			outcome, err := requestJobStatus(database, job, db.StatusQueued, ExecuteOptions{Timeout: timeout})
			if err != nil {
				return ensured, contacted, err
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

		if err := AppendJobToQueue(job, timeout); err != nil {
			if ssh.IsConnectionError(err.Error()) {
				return ensured, contacted, nil
			}
			return ensured, contacted, err
		}
		contacted = true
		if err := db.UpdateLastSyncedStatus(database, job.ID, db.StatusQueued); err != nil {
			return ensured, contacted, err
		}
		ensured++
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
		hfInputs = append(hfInputs, asset)
	}
	if len(hfInputs) == 0 {
		return nil
	}

	// Refresh target-host cache state first so we do not re-download assets that
	// are already present but missing from the local inventory DB.
	scanHFCacheDuringSync(database, host)

	plan, err := prestage.BuildPlan(database, host, inputs)
	if err != nil {
		return err
	}
	if err := prestage.Execute(database, plan, timeout); err != nil {
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

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
