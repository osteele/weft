package campaign

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func claimJobForLaunch(database *sql.DB, jobID, instanceID int64) (*db.Job, error) {
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
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
