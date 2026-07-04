package db

import (
	"database/sql"
	"testing"
)

// setupTestDB creates an isolated database from a pre-migrated schema template.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return SetupTestDB(t)
}

// setTestJobStatus updates the job_attempts table for test setup.
func setTestJobStatus(db *sql.DB, jobID int64, status string, extra ...interface{}) {
	db.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, status, jobID)
}

// setTestJobStatusWithEndTime updates job_attempts with status and end_time.
func setTestJobStatusWithEndTime(db *sql.DB, jobID int64, status string, endTime int64) {
	db.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, status, endTime, jobID)
}
