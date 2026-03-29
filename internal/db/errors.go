package db

import (
	"errors"
	"strings"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrJobNotFound is returned when a job ID does not exist in the database.
var ErrJobNotFound = errors.New("job not found")

// ErrJobAlreadyClaimed is returned by SetJobLaunchID when the job is already
// assigned to an active launch (launching/running/grace/completed).
var ErrJobAlreadyClaimed = errors.New("job already claimed by another launch")

// IsDatabaseLocked reports whether err is a SQLite SQLITE_BUSY error.
func IsDatabaseLocked(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code() == sqlite3.SQLITE_BUSY
	}
	return false
}

// isDuplicateColumnError checks if an error is a "duplicate column name" SQLite error.
func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// isNoSuchTable checks if an error is a "no such table" SQLite error.
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table:")
}
