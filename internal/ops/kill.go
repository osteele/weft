package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
)

// KillJob sets the pending status to killed and attempts to reconcile immediately.
// Uses the three-way merge model: sets pending_status as user intent, then
// tries to apply to remote. If successful, status is updated; if not, the
// pending_status remains for later reconciliation during sync.
func KillJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	return StopJob(database, job, db.StatusKilled, opts)
}

// StopJob stops a live (running/starting/paused) job, recording targetStatus
// (killed or canceled) as the user intent. The stop mechanics are identical
// for both intents; only the recorded terminal status differs, so a user
// cancel must land as canceled, not killed.
// Uses the three-way merge model: sets pending_status as user intent, then
// tries to apply to remote. If successful, status is updated; if not, the
// pending_status remains for later reconciliation during sync.
func StopJob(database *sql.DB, job *db.Job, targetStatus string, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}

	var requestOp, doneOp string
	var noun string
	switch targetStatus {
	case db.StatusKilled:
		requestOp, doneOp = oplog.OpJobKill, oplog.OpJobKilled
		noun = "kill"
	case db.StatusCanceled:
		requestOp, doneOp = oplog.OpJobCancel, oplog.OpJobCancel
		noun = "cancel"
	default:
		return Result{}, fmt.Errorf("unsupported stop status %q for job %s", targetStatus, ids.FormatJobID(job.ID))
	}
	verb := noun + "ed"

	if err := db.SetRequestedStatus(database, job.ID, targetStatus); err != nil {
		return Result{}, fmt.Errorf("set requested status: %w", err)
	}

	oplog.LogJob(requestOp, job.ID, job.Host, oplog.WithDetailf("%sing job", noun))

	outcome, err := requestJobStatus(database, job, targetStatus, opts)
	if err != nil {
		return Result{}, err
	}

	if !outcome.hostAvailable {
		oplog.LogJob(requestOp, job.ID, job.Host,
			oplog.WithDetailf("%s deferred: host unreachable", noun))
		return Result{
			Success:  true,
			Deferred: !outcome.hostAvailable,
			JobID:    job.ID,
			Message:  fmt.Sprintf("Job %s %s pending (host unreachable)", ids.FormatJobID(job.ID), noun),
		}, nil
	}

	if !outcome.resolved {
		return Result{}, fmt.Errorf("unable to reconcile %s for job %s (status: %s)", noun, ids.FormatJobID(job.ID), outcome.currentStatus)
	}

	switch outcome.currentStatus {
	case targetStatus:
		oplog.LogJob(doneOp, job.ID, job.Host,
			oplog.WithDetailf("%s via reconciliation", verb))
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s %s", ids.FormatJobID(job.ID), verb),
		}, nil
	case db.StatusCompleted:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s already completed", ids.FormatJobID(job.ID)),
		}, nil
	case db.StatusFailed, db.StatusDead, db.StatusKilled:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s already %s", ids.FormatJobID(job.ID), outcome.currentStatus),
		}, nil
	default:
		return Result{}, fmt.Errorf("job %s remains %s after %s request", ids.FormatJobID(job.ID), outcome.currentStatus, noun)
	}
}

// CancelQueuedJob cancels a queued job so it won't run when the queue drains to it.
// Uses the three-way merge model: sets pending_status to canceled and reconciles
// via the sync path. If the host is unreachable, the pending status remains for
// later reconciliation.
func CancelQueuedJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}

	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus != db.StatusQueued {
		return Result{}, fmt.Errorf("job %s (status: %s): %w", ids.FormatJobID(job.ID), effectiveStatus, ErrNotQueued)
	}

	if err := db.SetRequestedStatus(database, job.ID, db.StatusCanceled); err != nil {
		return Result{}, fmt.Errorf("set requested status: %w", err)
	}

	oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceling queued job"))

	// Hostless jobs have no remote queue/process to reconcile against. Persist
	// cancellation locally so status cannot bounce back to queued via probe/reconcile.
	if !job.HasInventoryHost() {
		if err := db.UpdateStatusAndLastSynced(database, job.ID, db.StatusCanceled); err != nil {
			return Result{}, err
		}
		if err := db.SetRequestedStatus(database, job.ID, db.StatusCanceled); err != nil {
			return Result{}, err
		}
		if err := db.ClearPendingStatus(database, job.ID); err != nil {
			return Result{}, err
		}
		oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceled (hostless local transition)"))
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s canceled", ids.FormatJobID(job.ID)),
		}, nil
	}

	outcome, err := requestJobStatus(database, job, db.StatusCanceled, opts)
	if err != nil {
		return Result{}, err
	}

	if !outcome.hostAvailable {
		oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host,
			oplog.WithDetail("cancel deferred: host unreachable"))
		return Result{
			Success:  true,
			Deferred: true,
			JobID:    job.ID,
			Message:  fmt.Sprintf("Job %s cancel pending (host unreachable)", ids.FormatJobID(job.ID)),
		}, nil
	}

	if !outcome.resolved {
		return Result{
			Success:  true,
			Deferred: true,
			JobID:    job.ID,
			Message:  fmt.Sprintf("Job %s cancel pending (remote state uncertain)", ids.FormatJobID(job.ID)),
		}, nil
	}

	switch outcome.currentStatus {
	case db.StatusCanceled:
		oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceled"))
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s canceled", ids.FormatJobID(job.ID)),
		}, nil
	case db.StatusCompleted:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s already completed", ids.FormatJobID(job.ID)),
		}, nil
	case db.StatusFailed, db.StatusDead, db.StatusKilled:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s already %s", ids.FormatJobID(job.ID), outcome.currentStatus),
		}, nil
	default:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s now %s", ids.FormatJobID(job.ID), outcome.currentStatus),
		}, nil
	}
}
