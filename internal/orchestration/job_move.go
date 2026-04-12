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
)

type JobMoveCallbacks struct {
	OnWarning func(message string)
	OnMoved   func(jobID int64, target string)
}

func (c JobMoveCallbacks) warningf(format string, args ...any) {
	if c.OnWarning != nil {
		c.OnWarning(fmt.Sprintf(format, args...))
	}
}

func (c JobMoveCallbacks) moved(jobID int64, target string) {
	if c.OnMoved != nil {
		c.OnMoved(jobID, target)
	}
}

func ResolveEligibleJobs(
	database *sql.DB,
	jobIDs []int64,
	project string,
	from string,
	unplacedOnly bool,
	callbacks JobMoveCallbacks,
) ([]*db.Job, error) {
	var jobs []*db.Job
	explicitJobList := len(jobIDs) > 0
	eligibleStatuses := []string{db.StatusQueued, db.StatusPendingPlacement}
	var sourceInstanceID int64
	sourceHost := ""
	if from != "" {
		trimmedFrom := strings.TrimSpace(from)
		if parsedID, err := ids.ParseInstanceID(trimmedFrom); err == nil {
			sourceInstanceID = parsedID
			selected, err := db.GetLaunchJobsIncludingAttempts(database, sourceInstanceID)
			if err != nil {
				return nil, fmt.Errorf("list jobs on instance %s: %w", ids.FormatInstanceID(sourceInstanceID), err)
			}
			for _, job := range selected {
				if job != nil && job.LaunchID != nil && *job.LaunchID == sourceInstanceID {
					jobs = append(jobs, job)
				}
			}
		} else {
			sourceHost = trimmedFrom
			selected, err := db.ListJobsByStatuses(database, eligibleStatuses, sourceHost, "", 0, nil, "")
			if err != nil {
				return nil, fmt.Errorf("list jobs on host %s: %w", sourceHost, err)
			}
			jobs = append(jobs, selected...)
		}
	} else if project != "" {
		selected, err := db.ListJobsByStatuses(database, eligibleStatuses, "", project, 0, nil, "")
		if err != nil {
			return nil, fmt.Errorf("list queued jobs: %w", err)
		}
		jobs = append(jobs, selected...)
	}
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return nil, fmt.Errorf("get job %d: %w", jobID, err)
		}
		if job != nil {
			jobs = append(jobs, job)
			continue
		}
		callbacks.warningf("Warning: job %d not found, skipping", jobID)
	}

	var eligible []*db.Job
	for _, job := range jobs {
		status := job.EffectiveStatus()
		if status != db.StatusQueued && status != db.StatusPendingPlacement {
			if explicitJobList {
				callbacks.warningf("Warning: job %d has status %s, skipping", job.ID, status)
			}
			continue
		}
		if unplacedOnly && job.TargetKind() != db.JobTargetUnplaced {
			if explicitJobList {
				callbacks.warningf("Warning: job %d is already placed, skipping", job.ID)
			}
			continue
		}
		eligible = append(eligible, job)
	}
	if len(eligible) == 0 {
		if sourceInstanceID > 0 {
			return nil, fmt.Errorf("no eligible queued jobs found on %s", ids.FormatInstanceID(sourceInstanceID))
		}
		if sourceHost != "" {
			return nil, fmt.Errorf("no eligible queued jobs found on host %s", sourceHost)
		}
		if unplacedOnly {
			return nil, fmt.Errorf("no eligible unplaced queued jobs")
		}
		return nil, fmt.Errorf("no eligible queued jobs to move")
	}
	return eligible, nil
}

func MoveJobsToHost(database *sql.DB, jobs []*db.Job, host string, callbacks JobMoveCallbacks) (int, error) {
	moved := 0
	for _, job := range jobs {
		if err := unplaceIfNeededForMove(database, job); err != nil {
			callbacks.warningf("Warning: unplace job %d failed: %v", job.ID, err)
			continue
		}
		if err := db.UpdateJobHost(database, job.ID, host); err != nil {
			callbacks.warningf("Warning: move job %d failed: %v", job.ID, err)
			continue
		}
		if err := db.SetPendingStatus(database, job.ID, db.StatusQueued); err != nil {
			callbacks.warningf("Warning: set pending status for job %d: %v", job.ID, err)
		}
		moved++
		callbacks.moved(job.ID, host)
	}
	if moved == 0 {
		return 0, fmt.Errorf("all %d job(s) failed to move to %s", len(jobs), host)
	}
	return moved, nil
}

func MoveJobsToInstance(database *sql.DB, jobs []*db.Job, instanceID int64, callbacks JobMoveCallbacks) (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return 0, fmt.Errorf("load config: %w", err)
	}
	r2Client, err := BuildR2Client(cfg)
	if err != nil {
		return 0, fmt.Errorf("R2 client: %w", err)
	}

	var ready []*db.Job
	for _, job := range jobs {
		if err := unplaceIfNeededForMove(database, job); err != nil {
			callbacks.warningf("Warning: unplace job %d failed: %v", job.ID, err)
			continue
		}
		ready = append(ready, job)
	}
	if len(ready) == 0 {
		return 0, fmt.Errorf("all jobs failed to unplace")
	}

	if err := campaign.SubmitJobsToInstance(context.Background(), database, r2Client, instanceID, ready); err != nil {
		return 0, fmt.Errorf("submit to instance %s: %w", ids.FormatInstanceID(instanceID), err)
	}
	target := "instance " + ids.FormatInstanceID(instanceID)
	for _, job := range ready {
		callbacks.moved(job.ID, target)
	}
	return len(ready), nil
}

func unplaceIfNeededForMove(database *sql.DB, job *db.Job) error {
	if job.TargetKind() == db.JobTargetUnplaced {
		return nil
	}
	_, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast))
	return err
}
