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
