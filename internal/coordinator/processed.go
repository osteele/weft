package coordinator

import (
	"database/sql"
	"time"
)

const processedIntentsSchema = `
CREATE TABLE IF NOT EXISTS processed_intents (
	intent_id TEXT PRIMARY KEY,
	processed_at INTEGER NOT NULL
);
`

// initProcessedTable creates the processed_intents table if it doesn't exist.
func initProcessedTable(db *sql.DB) error {
	_, err := db.Exec(processedIntentsSchema)
	return err
}

// markProcessed records an intent ID as processed in the database.
func markProcessed(db *sql.DB, intentID string) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO processed_intents (intent_id, processed_at) VALUES (?, ?)`,
		intentID, time.Now().Unix(),
	)
	return err
}

// isProcessed checks if an intent ID has already been processed.
func isProcessed(db *sql.DB, intentID string) bool {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM processed_intents WHERE intent_id = ?`,
		intentID,
	).Scan(&count)
	return err == nil && count > 0
}

// cleanupOldProcessed removes entries older than the given duration
// to prevent the table from growing indefinitely.
func cleanupOldProcessed(db *sql.DB, maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).Unix()
	_, err := db.Exec(`DELETE FROM processed_intents WHERE processed_at < ?`, cutoff)
	return err
}
