package ops

import (
	"database/sql"
	"log"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/remote"
	"github.com/osteele/weft/internal/ssh"
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
	if result.HostContacted {
		ensured, err := ensureQueuedJobsOnRemote(database, activeJobs, timeout)
		if err == nil {
			result.Updated += ensured
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
			if err == nil {
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

// ensureQueuedJobsOnRemote appends locally-queued jobs to the remote queue
// when they haven't been synced yet. This handles the case where a job was
// recorded locally while the host was unreachable, or where the batch status
// sync path didn't push the job to the remote queue.
func ensureQueuedJobsOnRemote(database *sql.DB, jobs []*db.Job, timeout time.Duration) (int, error) {
	ensured := 0
	for _, job := range jobs {
		if job.Status != db.StatusQueued || job.PendingStatus != nil || job.LastSyncedStatus == db.StatusQueued {
			continue
		}
		if err := AppendJobToQueue(job, timeout); err != nil {
			return ensured, err
		}
		if err := db.UpdateLastSyncedStatus(database, job.ID, db.StatusQueued); err != nil {
			return ensured, err
		}
		ensured++
	}
	return ensured, nil
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
