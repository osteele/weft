package queueblock

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

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
