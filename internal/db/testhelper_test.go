package db

import (
	"database/sql"
	"os"
	"testing"
)

// setupTestDB creates a temporary database for testing.
// Uses the dbPathMu mutex to prevent race conditions with parallel tests.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	// Create temp file for test database
	tmpFile, err := os.CreateTemp("", "weft-test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()

	// Use SetDBPath which handles mutex locking
	cleanup := SetDBPath(tmpFile.Name())
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmpFile.Name())
	})

	db, err := Open()
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db
}

// setTestJobStatus updates the job_attempts table for test setup.
func setTestJobStatus(db *sql.DB, jobID int64, status string, extra ...interface{}) {
	db.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, status, jobID)
}

// setTestJobStatusWithEndTime updates job_attempts with status and end_time.
func setTestJobStatusWithEndTime(db *sql.DB, jobID int64, status string, endTime int64) {
	db.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, status, endTime, jobID)
}
