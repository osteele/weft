package ops

import (
	"database/sql"

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
	queueName string,
	opts SyncOptions,
) (updated bool, err error) {
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
			updated = true
		}
	}()

	// Probe 1: Check if status file exists (job completed)
	completedResult, completionInfo := prober.ProbeCompleted(job.ID)
	if completedResult == remote.ProbeTrue && completionInfo != nil {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return false, err
		}
		if err := RecordJobCompletion(database, job.ID, completionInfo.ExitCode, completionInfo.EndTime); err != nil {
			return false, err
		}
		CacheCompletedJobLog(job)
		// Clear the current job marker on remote if it matches this job.
		clearCurrentJobIfMatches(job.Host, queueName, job.ID, timeout)
		return true, nil
	}

	// Probe 2: Check if job is the current job in queue runner
	currentResult := prober.ProbeCurrent(queueName, job.ID)
	if currentResult == remote.ProbeTrue {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return false, err
		}
		switch job.Status {
		case db.StatusQueued:
			if err := db.MarkQueuedJobRunning(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		case db.StatusStarting:
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			if err := db.MarkRunningFromTerminal(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	// Probe 3: Check if job is in queue file (waiting)
	inQueueResult := prober.ProbeInQueue(queueName, job.ID)
	if inQueueResult == remote.ProbeTrue {
		if job.PendingStatus != nil && (*job.PendingStatus == db.StatusCanceled || *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead) {
			if err := host.RemoveFromQueue(queueName, job.ID); err != nil {
				return false, err
			}
			finalStatus := db.StatusCanceled
			if *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead {
				finalStatus = db.StatusKilled
			}
			if err := db.ClearPendingAndUpdateStatus(database, job.ID, finalStatus); err != nil {
				return false, err
			}
			return true, nil
		}

		// Job is queued - if DB says running, fix it
		if job.Status == db.StatusRunning {
			if err := db.MarkQueuedByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	// Probe 4: Check if process is paused via PID
	pausedResult := prober.ProbeProcessPaused(job.ID)
	if pausedResult == remote.ProbeTrue {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return false, err
		}
		switch job.Status {
		case db.StatusQueued, db.StatusStarting, db.StatusRunning:
			if err := db.MarkPausedByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			if err := db.MarkPausedFromTerminal(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		case db.StatusPaused:
			return false, nil
		}
	}

	// Probe 5: Check if process is running via PID
	processResult := prober.ProbeProcessRunning(job.ID)
	if processResult == remote.ProbeTrue {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return false, err
		}
		switch job.Status {
		case db.StatusQueued, db.StatusStarting:
			// Job is running but not marked as "current" - this happens when
			// multiple jobs run concurrently and only one is tracked as "current"
			if err := db.MarkQueuedJobRunning(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		case db.StatusPaused:
			if err := db.MarkRunningFromPaused(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
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
			return false, nil
		}
		if err := startQueuedJobNow(database, job, queueName, timeout); err != nil {
			return false, err
		}
		return true, nil
	}

	// Ensure queued job is present in queue if it was recorded locally while host was unreachable.
	if inQueueResult == remote.ProbeFalse && job.Status == db.StatusQueued && job.PendingStatus == nil {
		entry := remote.QueueEntry{
			JobID:       job.ID,
			WorkingDir:  job.WorkingDir,
			Command:     job.Command,
			Description: job.Description,
			EnvVars:     job.EnvVars,
			DepSpec:     job.DepSpec,
		}
		if err := host.AppendToQueue(queueName, entry); err != nil {
			return false, err
		}
		if err := db.UpdateLastSyncedStatus(database, job.ID, db.StatusQueued); err != nil {
			return false, err
		}
		return true, nil
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
					return false, err
				}
				return true, nil
			case db.StatusKilled, db.StatusDead:
				if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusKilled); err != nil {
					return false, err
				}
				return true, nil
			}
		}
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return false, err
		}
		return true, nil
	}

	// At least one probe returned unknown - don't change status
	return false, nil
}
