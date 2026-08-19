package db

import (
	"database/sql"
	"fmt"
	"time"
)

// ListUnprocessedTerminalJobs returns the untombstoned jobs within maxAgeDays
// whose effective status is completed, failed, or dead and which do not carry
// the processed tag — the unprocessed terminal-job inbox. Killed and canceled
// jobs are terminal but not inbox rows.
//
// The SQL below is a pre-filter for row count, not a replacement for the
// predicate: it drops only rows that are definitely not inbox rows, and the
// Go tag filter applied here stays authoritative for exact tag semantics
// (canonicalization, legacy comma-separated encodings). If SQL and Go ever
// disagree, Go wins and the only cost is a wasted row.
func ListUnprocessedTerminalJobs(db *sql.DB, maxAgeDays int) ([]*Job, error) {
	query, args := unprocessedTerminalJobsQuery(maxAgeDays)
	jobs, err := queryJobs(db, query, args...)
	if err != nil {
		return nil, err
	}
	return FilterJobsByTags(jobs, nil, "unprocessed"), nil
}

func unprocessedTerminalJobsQuery(maxAgeDays int) (string, []interface{}) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE tombstoned = 0`, qualifiedJobSelectColumns("job_status"))
	args := []interface{}{}
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
