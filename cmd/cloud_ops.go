package cmd

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/orchestration"
)

// killOrCancelCloudJob handles kill/cancel for cloud jobs at the command layer.
// Returns (message, nil) if the cloud job was handled, ("", nil) if the job is
// not a cloud job (caller should proceed with normal logic), or ("", err) on failure.
func killOrCancelCloudJob(database *sql.DB, jobID int64, targetStatus string) (string, error) {
	return orchestration.KillOrCancelCloudJob(database, jobID, targetStatus)
}

// isCloudJob checks if a job is a cloud job using an existing database connection.
func isCloudJob(database *sql.DB, jobID int64) (bool, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return false, fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return false, nil // let the caller handle "not found"
	}
	return job.IsLaunchJob(), nil
}
