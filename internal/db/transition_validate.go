package db

import (
	"database/sql"

	"github.com/osteele/weft/internal/status"
)

// getAttemptStatus returns the current status of the latest attempt for a job.
// If openOnly is true, only open (non-ended) attempts are considered.
// Returns empty string if no matching attempt exists.
func getAttemptStatus(db *sql.DB, jobID int64, openOnly bool) string {
	query := `SELECT status FROM job_attempts WHERE job_id = ?`
	if openOnly {
		query += ` AND end_time IS NULL`
	}
	query += ` ORDER BY attempt_number DESC LIMIT 1`
	var s string
	if err := db.QueryRow(query, jobID).Scan(&s); err != nil {
		return ""
	}
	return s
}

// checkTransition validates a status transition and returns an error if invalid.
// For mutation functions that have no WHERE-clause status guard and would write
// bad state on an invalid transition.
func checkTransition(db *sql.DB, jobID int64, toStatus string, authoritative bool, source status.Source) error {
	from := getAttemptStatus(db, jobID, false)
	if from == "" {
		return nil
	}
	_, err := status.ValidateTransition(from, toStatus, authoritative)
	return err
}

// checkOpenTransition is like checkTransition but checks only open attempts.
func checkOpenTransition(db *sql.DB, jobID int64, toStatus string, authoritative bool, source status.Source) error {
	from := getAttemptStatus(db, jobID, true)
	if from == "" {
		return nil
	}
	_, err := status.ValidateTransition(from, toStatus, authoritative)
	return err
}

// warnTransition validates a status transition in warn mode.
// For mutation functions that already have WHERE-clause status guards —
// violations are logged but the SQL will safely match 0 rows.
func warnTransition(db *sql.DB, jobID int64, toStatus string, authoritative bool, source status.Source) {
	from := getAttemptStatus(db, jobID, false)
	if from != "" {
		status.WarnOnInvalid(from, toStatus, authoritative, source)
	}
}

// warnOpenTransition is like warnTransition but checks only open attempts.
func warnOpenTransition(db *sql.DB, jobID int64, toStatus string, authoritative bool, source status.Source) {
	from := getAttemptStatus(db, jobID, true)
	if from != "" {
		status.WarnOnInvalid(from, toStatus, authoritative, source)
	}
}
