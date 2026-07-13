package campaign

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// PriorAttempt captures (attempt_id, launch_id) of the source attempt that
// a move intent will supersede. Used by callers to write a cancel marker to
// the source launch's R2 grace bucket so the source agent drops the
// superseded attempt instead of running it.
type PriorAttempt struct {
	AttemptID int64
	LaunchID  int64
}

func priorAttemptFromMoveIntent(intent *db.MoveIntent) PriorAttempt {
	prev := PriorAttempt{}
	if intent == nil {
		return prev
	}
	if intent.SourceAttemptID != nil {
		prev.AttemptID = *intent.SourceAttemptID
	}
	if intent.SourceLaunchID != nil {
		prev.LaunchID = *intent.SourceLaunchID
	}
	return prev
}

func claimJobForLaunch(database *sql.DB, jobID, instanceID int64) (*db.Job, error) {
	return claimJobForLaunchWithOpts(database, jobID, instanceID, false)
}

// claimJobForLaunchTransfer is the move-path counterpart: the source's
// prior claim is superseded rather than rejected with ErrJobAlreadyClaimed.
// Only callers operating under an open MoveIntent should use this.
func claimJobForLaunchTransfer(database *sql.DB, jobID, instanceID int64) (*db.Job, error) {
	return claimJobForLaunchWithOpts(database, jobID, instanceID, true)
}

func claimJobForLaunchWithOpts(database *sql.DB, jobID, instanceID int64, transfer bool) (*db.Job, error) {
	var err error
	if transfer {
		err = db.TransferJobLaunchID(database, jobID, instanceID)
	} else {
		err = db.SetJobLaunchID(database, jobID, instanceID)
	}
	if err != nil {
		return nil, fmt.Errorf("set launch_id for job %s: %w", ids.FormatJobID(jobID), err)
	}
	updatedJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		return nil, fmt.Errorf("refresh job %s after cloud assignment: %w", ids.FormatJobID(jobID), err)
	}
	if updatedJob == nil {
		return nil, fmt.Errorf("refresh job %s after cloud assignment: job not found", ids.FormatJobID(jobID))
	}
	if updatedJob.LaunchID == nil || *updatedJob.LaunchID != instanceID {
		return nil, fmt.Errorf("job %s launch_id mismatch after claim: got %v, want %d", ids.FormatJobID(jobID), updatedJob.LaunchID, instanceID)
	}
	if updatedJob.LatestRunID == nil || *updatedJob.LatestRunID <= 0 {
		return nil, fmt.Errorf("job %s has invalid latest_run_id after claim: %v", ids.FormatJobID(jobID), updatedJob.LatestRunID)
	}
	return updatedJob, nil
}
