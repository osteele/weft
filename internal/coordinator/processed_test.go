package coordinator

import (
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestProcessedIntents_MarkAndCheck(t *testing.T) {
	db := setupCoordTestDB(t)

	// Initially not processed
	if isProcessed(db, "intent-1") {
		t.Error("intent-1 should not be processed initially")
	}

	// Mark as processed
	if err := markProcessed(db, "intent-1"); err != nil {
		t.Fatal(err)
	}

	// Now should be processed
	if !isProcessed(db, "intent-1") {
		t.Error("intent-1 should be processed after marking")
	}

	// Other intents still not processed
	if isProcessed(db, "intent-2") {
		t.Error("intent-2 should not be processed")
	}
}

func TestProcessedIntents_Idempotent(t *testing.T) {
	db := setupCoordTestDB(t)

	// Marking twice should not error (INSERT OR IGNORE)
	if err := markProcessed(db, "intent-1"); err != nil {
		t.Fatal(err)
	}
	if err := markProcessed(db, "intent-1"); err != nil {
		t.Fatal(err)
	}

	if !isProcessed(db, "intent-1") {
		t.Error("intent-1 should still be processed")
	}
}

func TestProcessedIntents_Cleanup(t *testing.T) {
	db := setupCoordTestDB(t)

	// Insert an entry with a very old timestamp
	_, err := db.Exec(
		`INSERT INTO processed_intents (intent_id, processed_at) VALUES (?, ?)`,
		"old-intent", time.Now().Add(-30*24*time.Hour).Unix(),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a recent entry
	if err := markProcessed(db, "recent-intent"); err != nil {
		t.Fatal(err)
	}

	// Cleanup entries older than 7 days
	if err := cleanupOldProcessed(db, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	// Old entry should be gone
	if isProcessed(db, "old-intent") {
		t.Error("old-intent should have been cleaned up")
	}

	// Recent entry should remain
	if !isProcessed(db, "recent-intent") {
		t.Error("recent-intent should still be present")
	}
}

func TestProcessedIntents_SurvivesReopen(t *testing.T) {
	db := setupCoordTestDB(t)

	if err := markProcessed(db, "persistent-intent"); err != nil {
		t.Fatal(err)
	}

	// Re-init the table (simulates restart)
	if err := initProcessedTable(db); err != nil {
		t.Fatal(err)
	}

	// Entry should survive
	if !isProcessed(db, "persistent-intent") {
		t.Error("processed intent should survive table re-init")
	}
}
