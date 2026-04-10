package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
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
		return "", fmt.Errorf("job %d not found", jobID)
	}
	if !job.IsLaunchJob() {
		return "", nil
	}

	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus == db.StatusQueued && job.LaunchID == nil {
		if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
			return "", err
		}
		return fmt.Sprintf("Job %d %s (was awaiting rental instance)", jobID, targetStatus), nil
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
		return fmt.Sprintf("Job %d %s%s", jobID, targetStatus, suffix), nil
	}

	if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
		return "", err
	}

	cfg, _ := config.Load()
	r2Client, err := BuildR2Client(cfg)
	if err != nil {
		return "", fmt.Errorf("R2 client: %w", err)
	}
	if r2Client == nil {
		return "", fmt.Errorf("R2 client is not configured")
	}

	killKey := r2keys.InstanceKillJob(inst.ID)
	if err := r2Client.PutObject(context.Background(), killKey, strings.NewReader(fmt.Sprintf("%d", jobID)), "text/plain"); err != nil {
		return "", fmt.Errorf("write kill signal to R2: %w", err)
	}
	return fmt.Sprintf("Job %d %s on rental instance (kill signal sent)", jobID, targetStatus), nil
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
		return ops.Result{}, fmt.Errorf("job %d not found", jobID)
	}

	opts := ops.OptionsForMode(mode)
	switch job.EffectiveStatus() {
	case db.StatusQueued:
		return ops.CancelQueuedJob(database, job, opts)
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return ops.KillJob(database, job, opts)
	default:
		return ops.Result{}, fmt.Errorf("job %d is %s; nothing to kill", job.ID, job.EffectiveStatus())
	}
}
