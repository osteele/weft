// Package sessioninbox provides the versioned machine contract for an
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
	ProjectRoot  string `json:"project_root,omitempty"`
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

const GroupsKind = "unprocessed_groups"

type GroupsScope struct {
	State string `json:"state"`
}

type Dispositions struct {
	CompletedOK    int `json:"completed_ok"`
	CompletedError int `json:"completed_error"`
	InfraSuspected int `json:"infra_suspected"`
	Dead           int `json:"dead"`
}

type Group struct {
	ProjectRoot         *string      `json:"project_root"`
	Project             string       `json:"project"`
	SubmitterSession    *string      `json:"submitter_session"`
	UnattributedSession bool         `json:"unattributed_session"`
	Dispositions        Dispositions `json:"dispositions"`
	Total               int          `json:"total"`
}

type GroupsQuery struct {
	Kind    string      `json:"kind"`
	Version int         `json:"version"`
	Scope   GroupsScope `json:"scope"`
	Groups  []Group     `json:"groups"`
}

// IsInboxJob reports membership in the narrow terminal-inbox unprocessed
// population: effective failed, dead, or completed. The wide list
// --unprocessed population also includes killed and canceled jobs; see
// QuerySessionUnprocessedInbox in specs/job-lifecycle.allium.
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

// Load derives the versioned machine contract from the scoped inbox rows.
func Load(database *sql.DB, project, submitterSession string, now time.Time) (Query, error) {
	scope, jobs, err := Rows(database, project, submitterSession)
	if err != nil {
		return Query{}, err
	}
	if err := db.PopulateProjectRoots(database, jobs); err != nil {
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
			ProjectRoot:  job.ProjectRoot,
			AgeSeconds:   ageSeconds,
			AgeBasis:     ageBasis,
			AgeTimestamp: ageTimestamp,
		})
	}
	query.Counts.Total = query.Counts.Completed + query.Counts.Failed
	sort.Slice(query.Jobs, func(i, j int) bool { return query.Jobs[i].id < query.Jobs[j].id })
	return query, nil
}

// LoadGroups derives the all-sessions grouped contract from the bounded inbox
// candidate query. It accepts no submitter-session argument by construction.
func LoadGroups(database *sql.DB) (GroupsQuery, error) {
	const maxAgeDays = 14
	jobs, err := db.ListAllUnprocessedTerminalJobs(database, maxAgeDays)
	if err != nil {
		return GroupsQuery{}, err
	}
	if err := db.PopulateSubmitterSessions(database, jobs); err != nil {
		return GroupsQuery{}, err
	}
	if err := db.PopulateProjectRoots(database, jobs); err != nil {
		return GroupsQuery{}, err
	}
	if err := db.PopulateRawAttemptStatuses(database, jobs); err != nil {
		return GroupsQuery{}, err
	}

	type groupKey struct {
		projectRoot      string
		project          string
		submitterSession string
	}
	groups := make(map[groupKey]*Group)
	for _, job := range jobs {
		if !IsInboxJob(job) {
			continue
		}
		key := groupKey{job.ProjectRoot, job.Project, job.SubmitterSession}
		group := groups[key]
		if group == nil {
			group = &Group{
				ProjectRoot:         optionalString(job.ProjectRoot),
				Project:             job.Project,
				SubmitterSession:    optionalString(job.SubmitterSession),
				UnattributedSession: job.SubmitterSession == "",
			}
			groups[key] = group
		}
		addDisposition(&group.Dispositions, job)
		group.Total++
	}

	query := GroupsQuery{
		Kind:    GroupsKind,
		Version: QueryVersion,
		Scope:   GroupsScope{State: "all_sessions"},
		Groups:  make([]Group, 0, len(groups)),
	}
	for _, group := range groups {
		query.Groups = append(query.Groups, *group)
	}
	sort.Slice(query.Groups, func(i, j int) bool {
		a, b := query.Groups[i], query.Groups[j]
		if (a.ProjectRoot == nil) != (b.ProjectRoot == nil) {
			return a.ProjectRoot != nil
		}
		if stringValue(a.ProjectRoot) != stringValue(b.ProjectRoot) {
			return stringValue(a.ProjectRoot) < stringValue(b.ProjectRoot)
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if (a.SubmitterSession == nil) != (b.SubmitterSession == nil) {
			return a.SubmitterSession != nil
		}
		return stringValue(a.SubmitterSession) < stringValue(b.SubmitterSession)
	})
	return query, nil
}

func addDisposition(counts *Dispositions, job *db.Job) {
	if job.EffectiveStatus() == db.StatusDead {
		counts.Dead++
		return
	}
	if job.ExitCode != nil && *job.ExitCode == 0 {
		counts.CompletedOK++
		return
	}
	if isInfraSuspectedReason(job.FailureReason) {
		counts.InfraSuspected++
		return
	}
	counts.CompletedError++
}

// isInfraSuspectedReason intentionally under-claims. failure_reason also
// stores arbitrary prose and log tails, so only exact machine-generated
// constants qualify; unknown text remains completed_error.
func isInfraSuspectedReason(reason string) bool {
	switch reason {
	case db.FailureReasonDiskFull,
		db.FailureReasonOOM,
		db.FailureReasonGPUOOM,
		db.FailureReasonSegfault,
		db.FailureReasonAborted,
		db.FailureReasonKilledSIGKILL,
		db.FailureReasonKilledSIGTERM,
		db.FailureReasonGPUIdle,
		db.FailureReasonStdoutSilence,
		db.FailureReasonSetupTimeout,
		db.FailureReasonRunTimeout,
		db.FailureReasonCUDADriverTooOld,
		db.FailureReasonInfraPrewarmDownloadFailed,
		db.FailureReasonInfraCloudArtifactStageFailed,
		db.FailureReasonInfraTorchPreflightFailed,
		db.FailureReasonInfraCUDAHardwareFault:
		return true
	}
	return false
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
