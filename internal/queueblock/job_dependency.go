package queueblock

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// TerminalDependencyFailure reports an explicit terminal user/job outcome.
// Infrastructure failures remain deferred because their producer may be
// relaunched; absence of a failure reason is likewise unknown, not proof that
// the dependency contract failed.
func TerminalDependencyFailure(parent *db.Job) (string, bool) {
	if parent == nil {
		return "", false
	}
	status := parent.EffectiveStatus()
	switch status {
	case db.StatusCanceled, db.StatusKilled, db.StatusSkipped:
		return status, true
	case db.StatusFailed:
		if db.IsInfraFailureReason(parent.FailureReason) {
			return "", false
		}
		return status, true
	default:
		return "", false
	}
}

// PropagateTerminalDependencySkips marks unplaced strict descendants skipped.
// It creates no attempt, is idempotent, and iterates so a skipped node can
// invalidate its own strict descendants in the same pass.
func PropagateTerminalDependencySkips(database *sql.DB) ([]int64, error) {
	if database == nil {
		return nil, nil
	}
	var skipped []int64
	for {
		jobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return skipped, err
		}
		changed := false
		for _, job := range jobs {
			if job == nil || job.EffectiveStatus() != db.StatusQueued {
				continue
			}
			for _, dep := range ParseJobDependencies(job.DepSpec) {
				if dep.AllowFailure {
					continue
				}
				parent, err := db.GetJobByID(database, dep.JobID)
				if err != nil || parent == nil {
					continue
				}
				outcome, terminal := TerminalDependencyFailure(parent)
				if !terminal {
					continue
				}
				reason := fmt.Sprintf("strict dependency %s ended %s", ids.FormatJobID(dep.JobID), outcome)
				marked, err := markDependencySkipped(database, job.ID, reason)
				if err != nil {
					return skipped, err
				}
				if marked {
					skipped = append(skipped, job.ID)
					changed = true
				}
				break
			}
		}
		if !changed {
			return skipped, nil
		}
	}
}

func markDependencySkipped(database *sql.DB, jobID int64, reason string) (bool, error) {
	tx, err := database.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE jobs
		SET requested_status = ?, placement_reasons = ?
		WHERE id = ? AND COALESCE(requested_status, '') NOT IN (?, ?, ?)`,
		db.StatusSkipped, reason, jobID, db.StatusSkipped, db.StatusCanceled, db.StatusKilled)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE job_attempts
		SET status = ?, end_time = COALESCE(end_time, strftime('%s','now')),
		    failure_reason = ?, error_message = ?
		WHERE id = (
			SELECT id FROM job_attempts WHERE job_id = ? AND end_time IS NULL
			ORDER BY attempt_number DESC, id DESC LIMIT 1
		)`, db.StatusCanceled, db.FailureReasonDependencyFailed, reason, jobID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// JobDependency is one job-ID dependency from jobs.dep_spec.
type JobDependency struct {
	JobID        int64
	AllowFailure bool
}

// ParseJobDependencies parses jobs.dep_spec entries such as "42" and
// "42:any". Malformed chunks are ignored for compatibility with the legacy
// queue parser.
func ParseJobDependencies(spec string) []JobDependency {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	chunks := strings.Split(spec, ",")
	deps := make([]JobDependency, 0, len(chunks))
	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		allowFailure := false
		if strings.HasSuffix(chunk, ":any") {
			allowFailure = true
			chunk = strings.TrimSuffix(chunk, ":any")
		}
		id, err := strconv.ParseInt(chunk, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		deps = append(deps, JobDependency{JobID: id, AllowFailure: allowFailure})
	}
	return deps
}

// JobDependenciesSatisfied reports whether every dependency in deps is
// satisfied. Strict dependencies require exit 0; AllowFailure dependencies
// require only a terminal upstream status.
func JobDependenciesSatisfied(database *sql.DB, deps []JobDependency) (string, bool) {
	for _, dep := range deps {
		depLabel := ids.FormatJobID(dep.JobID)
		depJob, err := db.GetJobByID(database, dep.JobID)
		if err != nil || depJob == nil {
			return fmt.Sprintf("dependency %s not found", depLabel), false
		}
		status := depJob.EffectiveStatus()
		if dep.AllowFailure {
			if db.IsTerminalStatus(status) {
				continue
			}
			return fmt.Sprintf("waiting for %s to finish (status: %s)", depLabel, status), false
		}
		switch {
		case status == db.StatusCompleted && depJob.ExitCode != nil && *depJob.ExitCode == 0:
			continue
		case status == db.StatusCompleted:
			return fmt.Sprintf("dependency %s exited non-zero; re-queue or switch to --after-any", depLabel), false
		case db.IsTerminalStatus(status):
			return fmt.Sprintf("dependency %s %s; re-queue or switch to --after-any", depLabel, status), false
		default:
			return fmt.Sprintf("waiting for %s to succeed (status: %s)", depLabel, status), false
		}
	}
	return "", true
}

// WaitingOnJobDependencyReason returns the first unsatisfied jobs.dep_spec
// reason for job, if any.
func WaitingOnJobDependencyReason(database *sql.DB, job *db.Job) (string, bool) {
	if database == nil || job == nil {
		return "", false
	}
	deps := ParseJobDependencies(job.DepSpec)
	if len(deps) == 0 {
		return "", false
	}
	reason, ok := JobDependenciesSatisfied(database, deps)
	return reason, !ok
}

// StrictDependencyLaunchIDs returns live rental launches that currently gate
// this job through strict --after edges. It is planning evidence only: callers
// must not claim or create an attempt for the blocked job.
func StrictDependencyLaunchIDs(database *sql.DB, job *db.Job) []int64 {
	if database == nil || job == nil {
		return nil
	}
	seen := map[int64]struct{}{}
	var launchIDs []int64
	for _, dep := range ParseJobDependencies(job.DepSpec) {
		if dep.AllowFailure {
			continue
		}
		parent, err := db.GetJobByID(database, dep.JobID)
		if err != nil || parent == nil || db.IsTerminalStatus(parent.EffectiveStatus()) || parent.LaunchID == nil || *parent.LaunchID <= 0 {
			continue
		}
		if _, ok := seen[*parent.LaunchID]; ok {
			continue
		}
		seen[*parent.LaunchID] = struct{}{}
		launchIDs = append(launchIDs, *parent.LaunchID)
	}
	return launchIDs
}

// HydrateWaitingOnJobDependencyReasons fills QueueBlockedReason on unplaced
// jobs that are still missing a reason, by inspecting --after / --after-any
// dependencies from jobs.dep_spec.
func HydrateWaitingOnJobDependencyReasons(database *sql.DB, jobs []*db.Job) {
	if database == nil || len(jobs) == 0 {
		return
	}
	for _, job := range jobs {
		if job == nil || strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		if !job.IsUnplacedAwaitingPlacement() {
			continue
		}
		if reason, blocked := WaitingOnJobDependencyReason(database, job); blocked {
			job.QueueBlockedReason = reason
		}
	}
}
