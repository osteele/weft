package ops

import (
	"database/sql"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/remote"
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
			if err == nil {
				result.Updated += updatedCount
				result.HostContacted = true
			}
			// Don't fail entire sync for batch error
		}
		for _, job := range tmuxJobs {
			syncResult, err := SyncJobQuick(database, job, syncOpts)
			if err != nil {
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
		for _, job := range activeJobs {
			seenJobs[job.ID] = true
			if job.PendingStatus != nil {
				_, err := SyncAndReconcile(database, job, ReconcileOptions{Timeout: syncOpts.Timeout})
				if err != nil {
					continue
				}
				result.HostContacted = true
				result.Updated++
			} else {
				syncResult, err := SyncJob(database, job, syncOpts)
				if err != nil {
					return result, err
				}
				if syncResult.HostContacted {
					result.HostContacted = true
				}
				if syncResult.Updated {
					result.Updated++
				}
			}
		}
	}

	// Step 2: Reconcile jobs with pending operations (kill, cancel, etc.)
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
			continue
		}
		if res != nil {
			result.HostContacted = true
			result.Updated++
		}
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
		_ = db.UpdateLastRestartCheck(database, host, checkTime)
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
				if !remote.IsConnectionError(err.Error()) {
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

	// Step 6: Record host sync time if we contacted the host
	if result.HostContacted {
		_ = db.RecordHostSync(database, host, time.Now())
	}

	return result, nil
}
