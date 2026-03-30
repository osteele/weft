package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2keys"
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

	if !job.IsLaunchJob() {
		return "", nil
	}

	// For queued rental jobs that have not yet been assigned to an instance, just update DB.
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus == db.StatusQueued && job.LaunchID == nil {
		if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
			return "", err
		}
		return fmt.Sprintf("Job %d %s (was awaiting rental instance)", jobID, targetStatus), nil
	}

	// Fetch instance
	var inst *db.Launch
	if job.LaunchID != nil {
		inst, _ = db.GetLaunch(database, *job.LaunchID)
	}

	// Instance missing or terminal — just update DB
	if inst == nil || campaign.IsInstanceTerminal(inst.Status) {
		if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
			return "", err
		}
		suffix := ""
		if inst != nil {
			suffix = " (cloud instance already terminated)"
		}
		return fmt.Sprintf("Job %d %s%s", jobID, targetStatus, suffix), nil
	}

	// Update DB first, then write kill signal to R2 for the agent to pick up.
	// This ordering ensures DB is always consistent — if R2 write fails, the
	// agent will still see the job as killed on next DB-based reconciliation.
	if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
		return "", err
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return "", fmt.Errorf("R2 client: %w", err)
	}

	killKey := r2keys.InstanceKillJob(inst.ID)
	if err := r2Client.PutObject(context.Background(), killKey, strings.NewReader(fmt.Sprintf("%d", jobID)), "text/plain"); err != nil {
		return "", fmt.Errorf("write kill signal to R2: %w", err)
	}

	return fmt.Sprintf("Job %d %s on rental instance (kill signal sent)", jobID, targetStatus), nil
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
