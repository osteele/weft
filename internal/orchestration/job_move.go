package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
	return ResolveEligibleJobsWithForce(database, jobIDs, project, from, unplacedOnly, false, callbacks)
}

// ResolveEligibleJobsWithForce is like ResolveEligibleJobs but, when
// allowRunning is true, also admits jobs in running/starting/paused state.
// Callers using this path move the jobs via the normal move pipeline, which
// uses TransferClaim to atomically supersede the source attempt and then
// calls TerminateForcedSources to SSH-kill the source process.
func ResolveEligibleJobsWithForce(
	database *sql.DB,
	jobIDs []int64,
	project string,
	from string,
	unplacedOnly bool,
	allowRunning bool,
	callbacks JobMoveCallbacks,
) ([]*db.Job, error) {
	var jobs []*db.Job
	explicitJobList := len(jobIDs) > 0
	eligibleStatuses := []string{db.StatusQueued, db.StatusPendingPlacement}
	if allowRunning {
		eligibleStatuses = append(eligibleStatuses, db.StatusRunning, db.StatusStarting, db.StatusPaused)
	}
	var sourceInstanceID int64
	sourceHost := ""
	sourceProject := ""
	// Per-source diagnostics for sharper "no eligible jobs" errors when the
	// user supplied --from <instance>. Distinguishes "instance unknown",
	// "instance has only historical jobs", and "instance has jobs but none
	// movable".
	var instanceMembershipTotal int
	var instanceCurrentlyAssigned []*db.Job
	if from != "" {
		trimmedFrom := strings.TrimSpace(from)
		if parsedID, err := ids.ParseInstanceID(trimmedFrom); err == nil {
			sourceInstanceID = parsedID
			// `GetLaunchJobsIncludingAttempts` returns rows for both current
			// and historical memberships, with a view-projected launch_id
			// that always matches sourceInstanceID. To distinguish "currently
			// assigned" from "only historical", read the membership-aware
			// list for the total and the strict launch-id-matched list for
			// the currently-assigned subset.
			membership, err := db.GetLaunchJobsIncludingAttempts(database, sourceInstanceID)
			if err != nil {
				return nil, fmt.Errorf("list jobs on instance %s: %w", ids.FormatInstanceID(sourceInstanceID), err)
			}
			instanceMembershipTotal = len(membership)
			currentlyAssigned, err := db.GetLaunchJobs(database, sourceInstanceID)
			if err != nil {
				return nil, fmt.Errorf("list currently-assigned jobs on instance %s: %w", ids.FormatInstanceID(sourceInstanceID), err)
			}
			for _, job := range currentlyAssigned {
				if job == nil {
					continue
				}
				instanceCurrentlyAssigned = append(instanceCurrentlyAssigned, job)
				jobs = append(jobs, job)
			}
		} else {
			sourceHost = trimmedFrom
			selected, err := db.ListJobsByStatuses(database, eligibleStatuses, sourceHost, "", 0, nil, "")
			if err != nil {
				return nil, fmt.Errorf("list jobs on host %s: %w", sourceHost, err)
			}
			jobs = append(jobs, selected...)
			if len(jobs) == 0 {
				hostJobs, err := db.ListJobsByStatuses(database, nil, sourceHost, "", 1, nil, "")
				if err != nil {
					return nil, fmt.Errorf("list jobs on host %s: %w", sourceHost, err)
				}
				if len(hostJobs) == 0 {
					sourceProject = trimmedFrom
					sourceHost = ""
					selected, err := db.ListJobsByStatuses(database, eligibleStatuses, "", sourceProject, 0, nil, "")
					if err != nil {
						return nil, fmt.Errorf("list queued jobs in project %s: %w", sourceProject, err)
					}
					jobs = append(jobs, selected...)
				}
			}
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
			return nil, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
		}
		if job != nil {
			jobs = append(jobs, job)
			continue
		}
		callbacks.warningf("Warning: job %s not found, skipping", ids.FormatJobID(jobID))
	}

	var eligible []*db.Job
	for _, job := range jobs {
		status := job.EffectiveStatus()
		if !isEligibleMoveStatus(status, allowRunning) {
			if explicitJobList {
				callbacks.warningf("Warning: job %s has status %s, skipping", ids.FormatJobID(job.ID), status)
			}
			continue
		}
		if unplacedOnly && job.TargetKind() != db.JobTargetUnplaced {
			if explicitJobList {
				callbacks.warningf("Warning: job %s is already placed, skipping", ids.FormatJobID(job.ID))
			}
			continue
		}
		eligible = append(eligible, job)
	}
	if len(eligible) == 0 {
		if sourceInstanceID > 0 {
			return nil, instanceSelectorError(sourceInstanceID, instanceMembershipTotal, instanceCurrentlyAssigned)
		}
		if sourceHost != "" {
			return nil, fmt.Errorf("no eligible queued jobs found on host %s", sourceHost)
		}
		if sourceProject != "" {
			return nil, fmt.Errorf("no eligible queued jobs found on host or project %s", sourceProject)
		}
		if unplacedOnly {
			return nil, fmt.Errorf("no eligible unplaced queued jobs")
		}
		return nil, fmt.Errorf("no eligible queued jobs to move")
	}
	return eligible, nil
}

// instanceSelectorError produces an actionable error for `--from wi<N>` when
// no eligible jobs were found, distinguishing three failure modes:
//
//  1. Instance is unknown to the local DB (no rows in launch_job_membership).
//  2. Instance has rows, but none are currently assigned to it (all jobs
//     were moved elsewhere or only attempted historically).
//  3. Instance has currently-assigned jobs, but none are queued or
//     pending_placement (so `move` cannot relocate them).
func instanceSelectorError(instanceID int64, membershipTotal int, assigned []*db.Job) error {
	label := ids.FormatInstanceID(instanceID)
	if membershipTotal == 0 {
		return fmt.Errorf("instance %s not found or has no associated jobs", label)
	}
	if len(assigned) == 0 {
		return fmt.Errorf("instance %s has %d historical job attempt(s) but no currently-assigned jobs", label, membershipTotal)
	}
	statusCounts := make(map[string]int, len(assigned))
	for _, job := range assigned {
		statusCounts[job.EffectiveStatus()]++
	}
	keys := make([]string, 0, len(statusCounts))
	for k := range statusCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, statusCounts[k]))
	}
	return fmt.Errorf("instance %s has %d job(s) but none are queued or pending_placement (%s)",
		label, len(assigned), strings.Join(parts, ", "))
}

func MoveJobsToHost(database *sql.DB, jobs []*db.Job, host string, callbacks JobMoveCallbacks) (int, error) {
	moved := 0
	for _, job := range jobs {
		if err := supersedeOpenPlacementForMove(database, job.ID); err != nil {
			callbacks.warningf("Warning: replace prior move for job %s failed: %v", ids.FormatJobID(job.ID), err)
			continue
		}
		if err := unplaceIfNeededForMove(database, job); err != nil {
			callbacks.warningf("Warning: unplace job %s failed: %v", ids.FormatJobID(job.ID), err)
			continue
		}
		if err := db.UpdateJobHost(database, job.ID, host); err != nil {
			callbacks.warningf("Warning: move job %s failed: %v", ids.FormatJobID(job.ID), err)
			continue
		}
		if err := db.SetPendingStatus(database, job.ID, db.StatusQueued); err != nil {
			callbacks.warningf("Warning: set pending status for job %s: %v", ids.FormatJobID(job.ID), err)
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
		if err := supersedeOpenPlacementForMove(database, job.ID); err != nil {
			callbacks.warningf("Warning: replace prior move for job %s failed: %v", ids.FormatJobID(job.ID), err)
			continue
		}
		if err := unplaceIfNeededForMove(database, job); err != nil {
			callbacks.warningf("Warning: unplace job %s failed: %v", ids.FormatJobID(job.ID), err)
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

func supersedeOpenPlacementForMove(database *sql.DB, jobID int64) error {
	if database == nil || jobID <= 0 {
		return nil
	}
	if intent, err := db.GetOpenMoveIntent(database, jobID); err != nil {
		return fmt.Errorf("get open move intent: %w", err)
	} else if intent != nil {
		if err := db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateCanceled, "superseded by explicit move"); err != nil {
			return fmt.Errorf("cancel open move intent: %w", err)
		}
	}
	if intent, err := db.GetOpenPlacementIntent(database, jobID); err != nil {
		return fmt.Errorf("get open placement intent: %w", err)
	} else if intent != nil {
		if err := db.ResolvePlacementIntent(database, intent.ID, db.PlacementIntentStateCanceled, "superseded by explicit move"); err != nil {
			return fmt.Errorf("cancel open placement intent: %w", err)
		}
	}
	return nil
}

// isEligibleMoveStatus reports whether a job's effective status lets it be
// admitted to the move pipeline. Queued/pending_placement always qualify; the
// running set is only admitted under --force.
func isEligibleMoveStatus(status string, allowRunning bool) bool {
	switch status {
	case db.StatusQueued, db.StatusPendingPlacement:
		return true
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return allowRunning
	default:
		return false
	}
}
