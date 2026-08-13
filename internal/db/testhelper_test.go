package db

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/osteele/weft/internal/db/migrations"
)

// setupTestDB creates an isolated database from a pre-migrated schema template.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return SetupTestDB(t)
}

// seedCurrentTestDBFile copies the cached, fully migrated schema into path.
// Tests that do not exercise migration history should use this instead of
// replaying every migration through Open.
func seedCurrentTestDBFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, testDBTemplate(t), 0o600); err != nil {
		t.Fatalf("seed current test database: %v", err)
	}
}

// seedPendingMigrationTestDBFile creates a current-schema database whose
// version record is one migration behind. The latest migrations are
// idempotent, so Open takes its migrated/startup-repair branch without every
// test paying to replay the full history under the race detector.
func seedPendingMigrationTestDBFile(t *testing.T, path string) {
	t.Helper()
	seedCurrentTestDBFile(t, path)
	database, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)", path))
	if err != nil {
		t.Fatalf("open pending-migration test database: %v", err)
	}
	setGooseVersionForTest(t, database, int(migrations.Target()-1))
	if err := database.Close(); err != nil {
		t.Fatalf("close pending-migration test database: %v", err)
	}
}

// setTestJobStatus updates the job_attempts table for test setup.
func setTestJobStatus(db *sql.DB, jobID int64, status string, extra ...interface{}) {
	db.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, status, jobID)
}

// setTestJobStatusWithEndTime updates job_attempts with status and end_time.
func setTestJobStatusWithEndTime(db *sql.DB, jobID int64, status string, endTime int64) {
	db.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, status, endTime, jobID)
}
