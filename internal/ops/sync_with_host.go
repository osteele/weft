package ops

import (
	"database/sql"

	"github.com/osteele/remote-jobs/internal/artifacts"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/remote"
)

// SyncQueueRunnerJobWithProber syncs a queue runner job using the provided Prober and Host.
// This is the testable version that accepts interfaces for mocking.
func SyncQueueRunnerJobWithProber(
	database *sql.DB,
	job *db.Job,
	prober remote.Prober,
	host remote.Host,
	opts SyncOptions,
) (result SyncResult, err error) {
	timeout := effectiveSyncTimeout(opts.Timeout)

	defer func() {
		if err != nil || opts.SkipSamples {
			return
		}
		metaUpdated, metaErr := updateJobCPUSamples(database, job, opts.Timeout)
		if metaErr != nil {
			err = metaErr
			return
		}
		if metaUpdated {
			result.Updated = true
		}
	}()

	// Probe 1: Check if status file exists (job completed)
	completedResult, completionInfo := prober.ProbeCompleted(job.ID)
	if completedResult == remote.ProbeTrue && completionInfo != nil {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		if err := RecordJobCompletion(database, job.ID, completionInfo.ExitCode, completionInfo.EndTime); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		CacheCompletedJobLog(job)
		// Fetch resource usage data (best-effort)
		_, _ = updateJobResourceUsage(database, job, timeout)
		// Clear the current job marker on remote if it matches this job.
		clearCurrentJobIfMatches(job.Host, job.ID, timeout)
		return SyncResult{Updated: true, HostContacted: true}, nil
	}

	// Probe 2: Check if job is the current job in queue runner
	currentResult := prober.ProbeCurrent(job.ID)
	if currentResult == remote.ProbeTrue {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		switch job.Status {
		case db.StatusQueued:
			if err := db.MarkQueuedJobRunning(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		case db.StatusStarting:
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			if err := db.MarkRunningFromTerminal(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		}
		return SyncResult{HostContacted: true}, nil
	}

	// Probe 3: Check if job is in queue file (waiting)
	inQueueResult := prober.ProbeInQueue(job.ID)
	if inQueueResult == remote.ProbeTrue {
		if job.PendingStatus != nil && (*job.PendingStatus == db.StatusCanceled || *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead) {
			if err := host.RemoveFromQueue(job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			finalStatus := db.StatusCanceled
			if *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead {
				finalStatus = db.StatusKilled
			}
			if err := db.ClearPendingAndUpdateStatus(database, job.ID, finalStatus); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		}

		// Job is queued - if DB says running, fix it
		if job.Status == db.StatusRunning {
			if err := db.MarkQueuedByID(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		}
		return SyncResult{HostContacted: true}, nil
	}

	// Probe 4: Check if process is paused via PID
	pausedResult := prober.ProbeProcessPaused(job.ID)
	if pausedResult == remote.ProbeTrue {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		switch job.Status {
		case db.StatusQueued, db.StatusStarting, db.StatusRunning:
			if err := db.MarkPausedByID(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			if err := db.MarkPausedFromTerminal(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		case db.StatusPaused:
			return SyncResult{HostContacted: true}, nil
		}
	}

	// Probe 5: Check if process is running via PID
	processResult := prober.ProbeProcessRunning(job.ID)
	if processResult == remote.ProbeTrue {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		switch job.Status {
		case db.StatusQueued, db.StatusStarting:
			// Job is running but not marked as "current" - this happens when
			// multiple jobs run concurrently and only one is tracked as "current"
			if err := db.MarkQueuedJobRunning(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		case db.StatusPaused:
			if err := db.MarkRunningFromPaused(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		}
		return SyncResult{HostContacted: true}, nil
	}

	// Handle pending start-now request
	// Only attempt if we got at least one definitive probe result (host is reachable).
	// If all probes returned unknown, the host is likely offline - don't flicker the status.
	hostReachable := completedResult != remote.ProbeUnknown ||
		currentResult != remote.ProbeUnknown ||
		inQueueResult != remote.ProbeUnknown ||
		pausedResult != remote.ProbeUnknown ||
		processResult != remote.ProbeUnknown

	if job.PendingStatus != nil && *job.PendingStatus == db.StatusRunning && job.Status == db.StatusQueued {
		if !hostReachable {
			// Host unreachable - leave pending status for retry when host comes online
			return SyncResult{HostContacted: false}, nil
		}
		if err := startQueuedJobNow(database, job, timeout); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	}

	// Ensure queued job is present in queue if it was recorded locally while host was unreachable.
	// Check LastSyncedStatus to avoid re-appending jobs that were already synced but not yet
	// processed by the queue runner (which would cause duplicate entries in the pending array).
	if inQueueResult == remote.ProbeFalse && job.Status == db.StatusQueued && job.PendingStatus == nil && job.LastSyncedStatus != db.StatusQueued {
		entry := remote.QueueEntry{
			JobID:        job.ID,
			WorkingDir:   job.WorkingDir,
			Command:      job.Command,
			Description:  job.Description,
			EnvVars:      artifacts.MergeEnvVars(job.EnvVars, job.ID),
			DepSpec:      job.DepSpec,
			CPUAllotment: job.CPUAllotment,
			Tags:         job.Tags,
		}
		if err := host.AppendToQueue(entry); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		if err := db.UpdateLastSyncedStatus(database, job.ID, db.StatusQueued); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	}

	// Only mark dead if ALL probes returned definitive false (not unknown)
	allDefinitelyFalse := completedResult == remote.ProbeFalse &&
		currentResult == remote.ProbeFalse &&
		inQueueResult == remote.ProbeFalse &&
		pausedResult == remote.ProbeFalse &&
		processResult == remote.ProbeFalse

	if allDefinitelyFalse {
		// If job has pending cancel/kill status and no remote state, honor the pending status
		if job.PendingStatus != nil {
			switch *job.PendingStatus {
			case db.StatusCanceled:
				if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusCanceled); err != nil {
					return SyncResult{HostContacted: true}, err
				}
				return SyncResult{Updated: true, HostContacted: true}, nil
			case db.StatusKilled, db.StatusDead:
				if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusKilled); err != nil {
					return SyncResult{HostContacted: true}, err
				}
				return SyncResult{Updated: true, HostContacted: true}, nil
			}
		}
		// Don't mark as dead if job was just queued locally - queue runner may not have processed it yet.
		// This handles the race condition where we just sent an add command but the queue runner
		// hasn't processed it, and the status file doesn't exist yet.
		if job.Status == db.StatusQueued && job.LastSyncedStatus == db.StatusQueued {
			return SyncResult{HostContacted: true}, nil
		}
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	}

	// At least one probe returned unknown - don't change status
	// Return whether host was actually reachable
	return SyncResult{HostContacted: hostReachable}, nil
}
