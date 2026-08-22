// Package sessioninbox provides the versioned, one-query contract for an
// agent session's unprocessed terminal jobs.
package sessioninbox

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

const QueryVersion = 1

type ScopeState string

const (
	ScopeUnscoped       ScopeState = "unscoped"
	ScopeScopedEmpty    ScopeState = "scoped_empty"
	ScopeScopedNonEmpty ScopeState = "scoped_nonempty"
)

type Scope struct {
	State            ScopeState `json:"state"`
	SubmitterSession string     `json:"submitter_session,omitempty"`
}

type Counts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// Job carries the minimum stable fields needed by session status lines.
// AgeBasis names the timestamp represented by AgeTimestamp.
type Job struct {
	id           int64
	JobID        string `json:"job_id"`
	Status       string `json:"status"`
	Project      string `json:"project"`
	AgeSeconds   int64  `json:"age_seconds"`
	AgeBasis     string `json:"age_basis"`
	AgeTimestamp int64  `json:"age_timestamp"`
}

type Query struct {
	Version int    `json:"version"`
	Scope   Scope  `json:"scope"`
	Counts  Counts `json:"counts"`
	Jobs    []Job  `json:"jobs"`
}

// IsInboxJob reports whether a row belongs in the unprocessed terminal-job
// inbox: an effective status of failed, dead, or completed. Killed and
// canceled jobs are terminal but are not inbox rows.
func IsInboxJob(job *db.Job) bool {
	if job == nil {
		return false
	}
	switch job.EffectiveStatus() {
	case db.StatusFailed, db.StatusDead, db.StatusCompleted:
		return true
	}
	return false
}

// IsFailedJob reports whether an inbox row counts as a failure: failed or
// dead outright, or completed with a non-zero exit code.
func IsFailedJob(job *db.Job) bool {
	if job == nil {
		return false
	}
	switch job.EffectiveStatus() {
	case db.StatusFailed, db.StatusDead:
		return true
	case db.StatusCompleted:
		return job.ExitCode != nil && *job.ExitCode != 0
	}
	return false
}

// Rows evaluates one local SQLite inbox query and returns the rows that
// classify as inbox jobs, with the scope those rows establish. State is
// derived from the classified rows, so ScopeScopedNonEmpty never accompanies
// an empty row set.
func Rows(database *sql.DB, project, submitterSession string) (Scope, []*db.Job, error) {
	const maxAgeDays = 14
	result, err := db.ListUnprocessedTerminalJobs(database, maxAgeDays, submitterSession)
	if err != nil {
		return Scope{}, nil, err
	}
	jobs := result.Jobs
	if project != "" {
		jobs = db.FilterJobsByProject(jobs, project)
	}
	scope, inbox := classify(result.Scoped, submitterSession, jobs)
	return scope, inbox, nil
}

// classify keeps the inbox rows and derives the scope those rows establish.
// State is read off the kept rows rather than the rows the SQL pre-filter
// returned, so ScopeScopedNonEmpty cannot accompany an empty row set.
func classify(scoped bool, submitterSession string, jobs []*db.Job) (Scope, []*db.Job) {
	inbox := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if IsInboxJob(job) {
			inbox = append(inbox, job)
		}
	}
	scope := Scope{State: ScopeUnscoped}
	if scoped {
		scope.SubmitterSession = submitterSession
		scope.State = ScopeScopedEmpty
		if len(inbox) > 0 {
			scope.State = ScopeScopedNonEmpty
		}
	}
	return scope, inbox
}

// Load derives the versioned machine contract from one inbox query, without
// further database reads.
func Load(database *sql.DB, project, submitterSession string, now time.Time) (Query, error) {
	scope, jobs, err := Rows(database, project, submitterSession)
	if err != nil {
		return Query{}, err
	}
	query := Query{
		Version: QueryVersion,
		Scope:   scope,
		Jobs:    make([]Job, 0, len(jobs)),
	}
	for _, job := range jobs {
		if IsFailedJob(job) {
			query.Counts.Failed++
		} else {
			query.Counts.Completed++
		}
		ageTimestamp, ageBasis := ageProvenance(job)
		ageSeconds := int64(0)
		if ageTimestamp > 0 {
			ageSeconds = max(0, now.Unix()-ageTimestamp)
		}
		query.Jobs = append(query.Jobs, Job{
			id:           job.ID,
			JobID:        ids.FormatJobID(job.ID),
			Status:       job.EffectiveStatus(),
			Project:      job.Project,
			AgeSeconds:   ageSeconds,
			AgeBasis:     ageBasis,
			AgeTimestamp: ageTimestamp,
		})
	}
	query.Counts.Total = query.Counts.Completed + query.Counts.Failed
	sort.Slice(query.Jobs, func(i, j int) bool { return query.Jobs[i].id < query.Jobs[j].id })
	return query, nil
}

// ReminderAdvice is the one instruction every unprocessed-inbox surface ends
// with, so the wording cannot drift between them.
const ReminderAdvice = "Use the process-results skill before marking them processed."

func FormatReminder(query Query) string {
	if query.Scope.State != ScopeScopedNonEmpty {
		return ""
	}
	const maxJobs = 3
	parts := make([]string, 0, min(maxJobs, len(query.Jobs))+1)
	for i, job := range query.Jobs {
		if i == maxJobs {
			break
		}
		parts = append(parts, job.JobID+" "+job.Status)
	}
	if remaining := len(query.Jobs) - len(parts); remaining > 0 {
		parts = append(parts, fmt.Sprintf("+%d more", remaining))
	}
	return fmt.Sprintf("This session has %d unprocessed terminal %s (%s). %s",
		query.Counts.Total, pluralizeJob(query.Counts.Total), strings.Join(parts, ", "), ReminderAdvice)
}

func ageProvenance(job *db.Job) (int64, string) {
	if job.EndTime != nil && *job.EndTime > 0 {
		return *job.EndTime, "end_time"
	}
	if job.StartTime > 0 {
		return job.StartTime, "start_time"
	}
	if job.CreatedAt > 0 {
		return job.CreatedAt, "created_at"
	}
	return 0, "unknown"
}

func pluralizeJob(n int) string {
	if n == 1 {
		return "job"
	}
	return "jobs"
}
