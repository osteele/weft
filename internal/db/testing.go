package db

import (
	"database/sql"
	"os"
	"testing"
)

// SetupTestDB creates a temporary database for testing
func SetupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	// Create temp file for test database
	tmpFile, err := os.CreateTemp("", "weft-db-test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()

	// Override db path for testing
	cleanup := SetDBPath(tmpFile.Name())
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmpFile.Name())
	})

	database, err := Open()
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	return database
}
