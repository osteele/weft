package campaign

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
)

func claimJobForLaunch(database *sql.DB, jobID, instanceID int64) (*db.Job, error) {
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		return nil, fmt.Errorf("set launch_id for job %d: %w", jobID, err)
	}
	updatedJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		return nil, fmt.Errorf("refresh job %d after cloud assignment: %w", jobID, err)
	}
	if updatedJob == nil {
		return nil, fmt.Errorf("refresh job %d after cloud assignment: job not found", jobID)
	}
	if updatedJob.LaunchID == nil || *updatedJob.LaunchID != instanceID {
		return nil, fmt.Errorf("job %d launch_id mismatch after claim: got %v, want %d", jobID, updatedJob.LaunchID, instanceID)
	}
	if updatedJob.LatestRunID == nil || *updatedJob.LatestRunID <= 0 {
		return nil, fmt.Errorf("job %d has invalid latest_run_id after claim: %v", jobID, updatedJob.LatestRunID)
	}
	return updatedJob, nil
}
