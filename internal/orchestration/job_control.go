package orchestration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2keys"
)

// KillOrCancelCloudJob handles kill/cancel for cloud jobs.
// Returns (message, nil) when handled, ("", nil) when not a cloud job.
func KillOrCancelCloudJob(database *sql.DB, jobID int64, targetStatus string) (string, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return "", fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return "", fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	if !job.IsLaunchJob() {
		return "", nil
	}
	if err := db.SetRequestedStatus(database, jobID, targetStatus); err != nil {
		return "", fmt.Errorf("set requested status: %w", err)
	}

	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus == db.StatusQueued && job.LaunchID == nil {
		if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
			return "", err
		}
		return fmt.Sprintf("Job %s %s (was awaiting rental instance)", ids.FormatJobID(jobID), targetStatus), nil
	}

	var inst *db.Launch
	if job.LaunchID != nil {
		inst, _ = db.GetLaunch(database, *job.LaunchID)
	}
	if inst == nil || campaign.IsInstanceTerminal(inst.Status) {
		if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
			return "", err
		}
		suffix := ""
		if inst != nil {
			suffix = " (cloud instance already terminated)"
		}
		return fmt.Sprintf("Job %s %s%s", ids.FormatJobID(jobID), targetStatus, suffix), nil
	}

	if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
		return "", err
	}
	return SignalCloudJobKill(context.Background(), jobID, inst.ID, targetStatus)
}

// SignalCloudJobKill writes the per-instance kill marker the agent polls.
//
// This is the one channel that reaches a running agent: the agent reads
// r2keys.InstanceKillJob rather than the database, so a local status write
// alone never stops work already dispatched. Callers that have established the
// job should not be running are expected to reach this directly — the
// job-level helpers above refuse a job whose effective status is already
// terminal, which is exactly the state a cancelled-but-running job is in.
//
// Recording the job's intent belongs to the caller, so a caller that must not
// escalate until the marker is known to be written can order the two itself.
func SignalCloudJobKill(ctx context.Context, jobID, launchID int64, targetStatus string) (string, error) {
	cfg, _ := config.Load()
	r2Client, err := BuildR2Client(cfg)
	if err != nil {
		return "", fmt.Errorf("R2 client: %w", err)
	}
	if r2Client == nil {
		return "", fmt.Errorf("R2 client is not configured")
	}

	killKey := r2keys.InstanceKillJob(launchID)
	if err := r2Client.PutObject(ctx, killKey, strings.NewReader(fmt.Sprintf("%d", jobID)), "text/plain"); err != nil {
		return "", fmt.Errorf("write kill signal to R2: %w", err)
	}
	return fmt.Sprintf("Job %s %s on rental instance (kill signal sent)", ids.FormatJobID(jobID), targetStatus), nil
}

// KillOrCancelJob routes cloud jobs through cloud control and non-cloud jobs through ops.
func KillOrCancelJob(database *sql.DB, jobID int64, targetStatus string, mode ops.TimeoutMode) (ops.Result, error) {
	if msg, err := KillOrCancelCloudJob(database, jobID, targetStatus); err != nil {
		return ops.Result{}, err
	} else if msg != "" {
		return ops.Result{Success: true, JobID: jobID, Message: msg}, nil
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return ops.Result{}, fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return ops.Result{}, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	if job.Backend == db.BackendSkyPilot {
		return ops.Result{}, fmt.Errorf("job %s is owned by SkyPilot; use an external-executor cancel path", ids.FormatJobID(jobID))
	}

	opts := ops.OptionsForMode(mode)
	switch job.EffectiveStatus() {
	case db.StatusQueued:
		return ops.CancelQueuedJob(database, job, opts)
	case db.StatusDraft:
		if err := db.SetRequestedStatus(database, job.ID, db.StatusCanceled); err != nil {
			return ops.Result{}, fmt.Errorf("set requested status: %w", err)
		}
		if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusCanceled); err != nil {
			return ops.Result{}, fmt.Errorf("update canceled status: %w", err)
		}
		return ops.Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s canceled", ids.FormatJobID(job.ID)),
		}, nil
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return ops.StopJob(database, job, targetStatus, opts)
	default:
		return ops.Result{}, fmt.Errorf("job %s is %s; nothing to kill", ids.FormatJobID(job.ID), job.EffectiveStatus())
	}
}

// signalCloudJobKillFunc is the sweep's seam over the R2 kill marker, so the
// selection, ordering and reporting logic is testable without cloud
// credentials. It deliberately covers only the marker write: the intent
// escalation that makes the sweep idempotent stays in production code, where a
// test can observe it.
var signalCloudJobKillFunc = SignalCloudJobKill

// cancelSweepSignalTimeout bounds one kill-marker write. The sweep runs inside
// every autopilot pass, and the AWS SDK's default dial timeout and retries
// would otherwise let a single unreachable instance stall the pass for over a
// minute, serially, for every runner sharing the pass.
const cancelSweepSignalTimeout = 15 * time.Second

// StopJobsStartedAfterCancel terminates work that began after its job's cancel
// was requested, and reports how many it signalled.
//
// A cancel writes local state; it cannot recall a launch already dispatched,
// because the job travels in the instance payload and the agent never
// re-reads cancellation. So the cancel's effect has to be completed later,
// when the start becomes observable. Detection is db.JobsStartedAfterCancel,
// which requires a recorded cancel time strictly before an observed start —
// terminating on anything weaker would kill live work on absent evidence.
//
// Errors stopping one job do not abort the sweep: each is independent, and a
// job that cannot be reached now is retried on the next pass rather than
// blocking the others. That retry is why the intent is escalated only once the
// marker is written: escalating first drops the job out of
// db.JobsStartedAfterCancel whether or not the kill ever landed, abandoning a
// cancel the user asked for on nothing more than a timed-out write. The retry
// is bounded without needing a counter, because selection ends as soon as the
// attempt reaches a terminal status.
//
// Only a cloud attempt carries an instance to signal. An on-prem attempt
// records no launch, so no agent polls a marker for it: the job is landed
// locally and reported, because the remote process is still running. Stopping
// it would need a bounded ops.StopJob and is not done here.
//
// The count is what was signalled. A job that could not be reached is reported
// rather than counted, because a sweep that counted attempts would report
// success for work still running.
func StopJobsStartedAfterCancel(database *sql.DB) (int, error) {
	starts, err := db.JobsStartedAfterCancel(database)
	if err != nil {
		return 0, fmt.Errorf("find jobs started after cancel: %w", err)
	}
	stopped := 0
	var failures []error
	for _, start := range starts {
		oplog.LogJob(oplog.OpJobKill, start.JobID, "", oplog.WithDetailf(
			"started %s after cancel requested %s; stopping work the user canceled",
			start.StartedAt.Format(time.RFC3339), start.CancelRequestedAt.Format(time.RFC3339)))
		if start.LaunchID == nil {
			if err := landUnsignalledCancel(database, start.JobID); err != nil {
				failures = append(failures, fmt.Errorf("job %s: %w", ids.FormatJobID(start.JobID), err))
				continue
			}
			failures = append(failures, fmt.Errorf("job %s started after its cancel with no instance recorded; marked killed locally, but no agent could be signalled and any remote process is still running", ids.FormatJobID(start.JobID)))
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), cancelSweepSignalTimeout)
		_, signalErr := signalCloudJobKillFunc(ctx, start.JobID, *start.LaunchID, db.StatusKilled)
		cancel()
		if signalErr != nil {
			failures = append(failures, fmt.Errorf("job %s: %w", ids.FormatJobID(start.JobID), signalErr))
			continue
		}
		if err := db.SetRequestedStatus(database, start.JobID, db.StatusKilled); err != nil {
			failures = append(failures, fmt.Errorf("job %s: record killed intent: %w", ids.FormatJobID(start.JobID), err))
			continue
		}
		stopped++
	}
	return stopped, errors.Join(failures...)
}

// landUnsignalledCancel records a post-cancel start that has no agent to
// signal as killed locally.
//
// The attempt status is written as well as the intent. Intent alone leaves the
// job reading killed through job_status while its latest attempt stays open
// with no end_time and no agent left to close it — the shape
// TerminalJobsHaveEndTime exists to exclude. The attempt closes first: if that
// write fails the job keeps its cancel intent and the next pass retries it,
// whereas escalating the intent first would strand the open attempt.
func landUnsignalledCancel(database *sql.DB, jobID int64) error {
	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusKilled); err != nil {
		return fmt.Errorf("close attempt as killed: %w", err)
	}
	if err := db.SetRequestedStatus(database, jobID, db.StatusKilled); err != nil {
		return fmt.Errorf("record killed intent: %w", err)
	}
	return nil
}
