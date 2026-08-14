package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
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
		return "", fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	if !job.IsLaunchJob() {
		return "", nil
	}
	if err := db.SetRequestedStatus(database, jobID, targetStatus); err != nil {
		return "", fmt.Errorf("set requested status: %w", err)
	}

	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus == db.StatusQueued && job.LaunchID == nil {
		if err := db.UpdateStatusAndLastSynced(database, jobID, targetStatus); err != nil {
			return "", err
		}
		return fmt.Sprintf("Job %s %s (was awaiting rental instance)", ids.FormatJobID(jobID), targetStatus), nil
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
		return fmt.Sprintf("Job %s %s%s", ids.FormatJobID(jobID), targetStatus, suffix), nil
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
	return fmt.Sprintf("Job %s %s on rental instance (kill signal sent)", ids.FormatJobID(jobID), targetStatus), nil
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
		return ops.Result{}, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	if job.Backend == db.BackendSkyPilot {
		return ops.Result{}, fmt.Errorf("job %s is owned by SkyPilot; use an external-executor cancel path", ids.FormatJobID(jobID))
	}

	opts := ops.OptionsForMode(mode)
	switch job.EffectiveStatus() {
	case db.StatusQueued:
		return ops.CancelQueuedJob(database, job, opts)
	case db.StatusDraft:
		if err := db.SetRequestedStatus(database, job.ID, db.StatusCanceled); err != nil {
			return ops.Result{}, fmt.Errorf("set requested status: %w", err)
		}
		if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusCanceled); err != nil {
			return ops.Result{}, fmt.Errorf("update canceled status: %w", err)
		}
		return ops.Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s canceled", ids.FormatJobID(job.ID)),
		}, nil
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return ops.StopJob(database, job, targetStatus, opts)
	default:
		return ops.Result{}, fmt.Errorf("job %s is %s; nothing to kill", ids.FormatJobID(job.ID), job.EffectiveStatus())
	}
}
