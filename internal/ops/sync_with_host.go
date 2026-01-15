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

	// Probe 4: Check if process is running via PID
	processResult := prober.ProbeProcessRunning(job.ID)
	if processResult == remote.ProbeTrue {
		if job.Status == db.StatusDead || job.Status == db.StatusFailed || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	// Handle pending start-now request
	if job.PendingStatus != nil && *job.PendingStatus == db.StatusRunning && job.Status == db.StatusQueued {
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
		processResult == remote.ProbeFalse

	if allDefinitelyFalse {
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return false, err
		}
		return true, nil
	}

	// At least one probe returned unknown - don't change status
	return false, nil
}
