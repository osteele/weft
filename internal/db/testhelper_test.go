package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/osteele/weft/internal/db/migrations"
)

var (
	testDBTemplateOnce  sync.Once
	testDBTemplateBytes []byte
	testDBTemplateErr   error
)

// setupTestDB creates an isolated database from a pre-migrated schema template.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	// Create temp file for test database
	tmpFile, err := os.CreateTemp("", "weft-test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	templateBytes := testDBTemplate(t)
	if err := os.WriteFile(tmpFile.Name(), templateBytes, 0o600); err != nil {
		t.Fatalf("Failed to seed test database: %v", err)
	}

	// Use SetDBPath which handles mutex locking
	cleanup := SetDBPath(tmpFile.Name())
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmpFile.Name())
	})

	// These databases are per-test throwaways. Keep FK checks, but skip
	// durability work that only matters for production database files.
	connStr := fmt.Sprintf("file:%s?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=foreign_keys(ON)&_txlock=immediate", tmpFile.Name())
	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db
}

func testDBTemplate(t *testing.T) []byte {
	t.Helper()
	testDBTemplateOnce.Do(func() {
		testDBTemplateBytes, testDBTemplateErr = buildTestDBTemplate()
	})
	if testDBTemplateErr != nil {
		t.Fatalf("build test database template: %v", testDBTemplateErr)
	}
	return testDBTemplateBytes
}

func buildTestDBTemplate() ([]byte, error) {
	tmpFile, err := os.CreateTemp("", "weft-test-template-*.db")
	if err != nil {
		return nil, fmt.Errorf("create template database: %w", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	connStr := fmt.Sprintf("file:%s?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=foreign_keys(ON)&_txlock=immediate", tmpFile.Name())
	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("open template database: %w", err)
	}
	if err := migrations.Up(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize template database: %w", err)
	}
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("close template database: %w", err)
	}
	bytes, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		return nil, fmt.Errorf("read template database: %w", err)
	}
	return bytes, nil
}

// setTestJobStatus updates the job_attempts table for test setup.
func setTestJobStatus(db *sql.DB, jobID int64, status string, extra ...interface{}) {
	db.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, status, jobID)
}

// setTestJobStatusWithEndTime updates job_attempts with status and end_time.
func setTestJobStatusWithEndTime(db *sql.DB, jobID int64, status string, endTime int64) {
	db.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, status, endTime, jobID)
}
