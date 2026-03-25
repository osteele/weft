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

// setTestJobStatus updates both jobs and job_attempts tables for test setup.
// Tests that directly set job status must use this to keep both tables in sync.
func setTestJobStatus(db *sql.DB, jobID int64, status string, extra ...interface{}) {
	db.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, status, jobID)
	db.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, status, jobID)
}

// setTestJobStatusWithEndTime updates both tables with status and end_time.
func setTestJobStatusWithEndTime(db *sql.DB, jobID int64, status string, endTime int64) {
	db.Exec(`UPDATE jobs SET status = ?, end_time = ? WHERE id = ?`, status, endTime, jobID)
	db.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, status, endTime, jobID)
}
