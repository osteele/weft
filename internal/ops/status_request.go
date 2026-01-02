package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
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
