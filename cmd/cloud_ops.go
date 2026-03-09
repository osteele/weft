package cmd

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

// killOrCancelCloudJob handles kill/cancel for cloud jobs at the command layer.
// Returns (message, nil) if the cloud job was handled, ("", nil) if the job is
// not a cloud job (caller should proceed with normal logic), or ("", err) on failure.
func killOrCancelCloudJob(database *sql.DB, jobID int64, targetStatus string) (string, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return "", fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return "", fmt.Errorf("job %d not found", jobID)
	}

	if !job.IsCloudJob() {
		return "", nil
	}

	// For queued cloud jobs with no instance or unplaced, just update DB
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus == db.StatusQueued && (job.CloudInstanceID == nil || job.Host == "") {
		if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
			return "", err
		}
		return fmt.Sprintf("Job %d %s (was awaiting cloud instance)", jobID, targetStatus), nil
	}

	// Fetch instance once, share with KillCloudJob
	var inst *db.CloudInstance
	if job.CloudInstanceID != nil {
		inst, _ = db.GetCloudInstance(database, *job.CloudInstanceID)
	}

	client := cloudClientForDBInstance("")
	if inst != nil {
		client = cloudClientForDBInstance(inst.Provider)
	}

	wasTerminal, err := ops.KillCloudJob(database, job, inst, client, ops.DefaultOptions().Timeout)
	if err != nil {
		return "", err
	}

	if wasTerminal {
		return fmt.Sprintf("Job %d %s (cloud instance already terminated)", jobID, targetStatus), nil
	}
	return fmt.Sprintf("Job %d %s on cloud instance", jobID, targetStatus), nil
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
	return job.IsCloudJob(), nil
}
