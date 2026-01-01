package ops

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// statusRequestOutcome captures the result of requesting a state transition.
type statusRequestOutcome struct {
	jobID         int64
	targetStatus  string
	hostAvailable bool
	resolved      bool
	currentStatus string
}

// requestJobStatus records the desired status transition and immediately attempts
// to reconcile the job on the remote host. Connection failures are treated as
// deferred work (resolved = false, hostAvailable = false).
func requestJobStatus(database *sql.DB, job *db.Job, targetStatus string, opts ExecuteOptions) (statusRequestOutcome, error) {
	outcome := statusRequestOutcome{
		jobID:        job.ID,
		targetStatus: targetStatus,
		currentStatus: func() string {
			if job == nil {
				return ""
			}
			return job.Status
		}(),
	}

	if err := db.SetPendingStatus(database, job.ID, targetStatus); err != nil {
		return outcome, fmt.Errorf("set pending status: %w", err)
	}

	// Reload job so reconciliation sees the pending state.
	refreshed, err := db.GetJobByID(database, job.ID)
	if err != nil {
		return outcome, fmt.Errorf("reload job: %w", err)
	}
	if refreshed == nil {
		return outcome, fmt.Errorf("job %d not found after setting pending status", job.ID)
	}
	job = refreshed
	outcome.currentStatus = job.Status

	reconcileOpts := ReconcileOptions{
		Timeout: opts.Timeout,
		Policy:  TerminalWins,
	}

	res, err := SyncAndReconcile(database, job, reconcileOpts)
	if err != nil {
		// Connection errors leave the request pending for later sync.
		if ssh.IsConnectionError(err.Error()) {
			return outcome, nil
		}
		return outcome, err
	}

	if res != nil && res.Error != nil {
		if ssh.IsConnectionError(res.Error.Error()) {
			return outcome, nil
		}
	}

	outcome.hostAvailable = true

	updated, err := db.GetJobByID(database, job.ID)
	if err != nil {
		return outcome, fmt.Errorf("reload job after sync: %w", err)
	}
	if updated != nil {
		outcome.currentStatus = updated.Status
		outcome.resolved = updated.PendingStatus == nil
	}

	return outcome, nil
}

// ConvertQueuedJobToDraft records draft intent and attempts immediate reconciliation.
func ConvertQueuedJobToDraft(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}
	if job.Status != db.StatusQueued {
		return Result{}, fmt.Errorf("job %d is not queued (status: %s)", job.ID, job.Status)
	}

	oplog.LogJob(oplog.OpJobDraft, job.ID, job.Host, oplog.WithDetail("queued → draft"))

	// Remove incompatible deferred operations (queue append or start-now signals)
	_, _ = db.DeletePendingOperationsForJob(database, job.ID, db.OpQueueJob, db.OpStartQueuedJob)

	outcome, err := requestJobStatus(database, job, db.StatusDraft, opts)
	if err != nil {
		return Result{}, err
	}

	if !outcome.hostAvailable {
		return Result{
			Success:  true,
			Deferred: true,
			JobID:    job.ID,
			Message:  fmt.Sprintf("Job %d draft pending (host unreachable)", job.ID),
		}, nil
	}

	if !outcome.resolved {
		return Result{}, fmt.Errorf("job %d is %s on %s, can't convert to draft", job.ID, outcome.currentStatus, job.Host)
	}

	if outcome.currentStatus != db.StatusDraft {
		return Result{}, fmt.Errorf("job %d is %s on %s, can't convert to draft", job.ID, outcome.currentStatus, job.Host)
	}

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %d converted to draft", job.ID),
	}, nil
}

// applyDraftRemoval removes the queued job entry from the remote queue file.
func applyDraftRemoval(job *db.Job, timeout time.Duration) error {
	queueName := job.QueueName
	if queueName == "" {
		queueName = DefaultQueueName
	}
	return removeFromQueueFile(job.Host, queueName, job.ID, timeout)
}

// finalizeDraftTransition clears pending intent, updates status, and removes queue metadata.
func finalizeDraftTransition(database *sql.DB, jobID int64) error {
	if err := db.ClearPendingAndUpdateStatus(database, jobID, db.StatusDraft); err != nil {
		return err
	}
	return db.ClearQueueAssignment(database, jobID)
}
