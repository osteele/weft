package db

import (
	"database/sql"
	"time"
)

// ProviderStatusTransition records an observed change in a cloud instance's
// provider-level status (e.g., "created" → "loading" → "running").
type ProviderStatusTransition struct {
	ID              int64
	CloudInstanceID int64
	ObservedAt      int64  // unix timestamp
	OldStatus       string // "" for the first observation
	NewStatus       string
}

// InsertProviderStatusTransition records a provider status change for a cloud instance.
func InsertProviderStatusTransition(database *sql.DB, cloudInstanceID int64, observedAt time.Time, oldStatus, newStatus string) error {
	_, err := database.Exec(
		`INSERT INTO provider_status_transitions (cloud_instance_id, observed_at, old_status, new_status) VALUES (?, ?, ?, ?)`,
		cloudInstanceID, observedAt.Unix(), oldStatus, newStatus,
	)
	return err
}

// GetProviderStatusTransitions returns all recorded transitions for a cloud instance, ordered by time.
func GetProviderStatusTransitions(database *sql.DB, cloudInstanceID int64) ([]ProviderStatusTransition, error) {
	rows, err := database.Query(
		`SELECT id, cloud_instance_id, observed_at, old_status, new_status FROM provider_status_transitions WHERE cloud_instance_id = ? ORDER BY observed_at ASC`,
		cloudInstanceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var transitions []ProviderStatusTransition
	for rows.Next() {
		var t ProviderStatusTransition
		if err := rows.Scan(&t.ID, &t.CloudInstanceID, &t.ObservedAt, &t.OldStatus, &t.NewStatus); err != nil {
			return nil, err
		}
		transitions = append(transitions, t)
	}
	return transitions, rows.Err()
}
