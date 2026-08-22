package db

import (
	"database/sql"
	"fmt"
	"time"
)

// UnprocessedTerminalJobs is the result of attributing the terminal-job inbox
// to one exact submitter session. Scoped is false when no session id was
// available, so an unknown inbox cannot be mistaken for an empty one.
type UnprocessedTerminalJobs struct {
	Scoped bool
	Jobs   []*Job
}

// ListUnprocessedTerminalJobs returns the untombstoned jobs within maxAgeDays
// that belong to submitterSession, whose effective status is completed,
// failed, or dead, and which do not carry the processed tag. Killed and
// canceled jobs are terminal but not inbox rows. An empty submitterSession
// returns an unscoped result without querying or inferring an owner.
//
// The SQL below is a pre-filter for row count, not a replacement for the
// predicate: it drops only rows that are definitely not inbox rows, and the
// Go tag filter applied here stays authoritative for exact tag semantics
// (canonicalization, legacy comma-separated encodings). If SQL and Go ever
// disagree, Go wins and the only cost is a wasted row.
func ListUnprocessedTerminalJobs(db *sql.DB, maxAgeDays int, submitterSession string) (UnprocessedTerminalJobs, error) {
	if submitterSession == "" {
		return UnprocessedTerminalJobs{Jobs: []*Job{}}, nil
	}
	query, args := unprocessedTerminalJobsQuery(maxAgeDays, submitterSession)
	jobs, err := queryJobs(db, query, args...)
	if err != nil {
		return UnprocessedTerminalJobs{}, err
	}
	return UnprocessedTerminalJobs{
		Scoped: true,
		Jobs:   FilterJobsByTags(jobs, nil, "unprocessed"),
	}, nil
}

func unprocessedTerminalJobsQuery(maxAgeDays int, submitterSession string) (string, []interface{}) {
	// The IS NOT NULL / != '' terms are redundant against the bound
	// parameter and must stay. idx_jobs_submitter_session is a partial
	// index carrying the same predicates, and SQLite uses a partial index
	// only where the query proves the index's WHERE clause. A bound
	// parameter could be null or empty, so equality alone does not prove
	// it and the planner falls back to scanning jobs.
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE tombstoned = 0 AND id IN (SELECT id FROM jobs WHERE submitter_session = ? AND submitter_session IS NOT NULL AND submitter_session != '')`, qualifiedJobSelectColumns("job_status"))
	args := []interface{}{submitterSession}
	if maxAgeDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -maxAgeDays).Unix()
		query += ` AND (start_time > ? OR start_time IS NULL OR start_time = 0)`
		args = append(args, cutoff)
	}
	// Effective status (Job.EffectiveStatus): a terminal status wins
	// outright; otherwise pending_status overrides. The unplaced remap of
	// running/starting/paused to queued cannot produce a terminal status, so
	// it does not appear here. job_attempts.status is NOT NULL by schema, so
	// the NOT IN disjunct is total. Killed and canceled fall out of both
	// disjuncts.
	query += ` AND (status IN ('completed', 'failed', 'dead')` +
		` OR (status NOT IN ('completed', 'dead', 'failed', 'killed', 'canceled')` +
		` AND pending_status IN ('completed', 'failed', 'dead')))`
	// A row is definitely processed when its tags contain the exact JSON
	// element "processed" — CanonicalizeTag maps no alias to "processed" and
	// never lowercases, so the comparison is exact. The backslash disjunct
	// keeps any tags containing JSON escape sequences for the Go filter,
	// because an escaped quote could otherwise fake the quoted substring.
	// Legacy comma-separated encodings carry no quotes and never match, so
	// they also fall through to Go.
	query += ` AND (tags IS NULL OR tags NOT LIKE '%"processed"%' OR tags LIKE '%\%')`
	query += ` ORDER BY CASE WHEN status IN ('running', 'starting', 'paused') THEN 0 ELSE 1 END, id DESC`
	return query, args
}
