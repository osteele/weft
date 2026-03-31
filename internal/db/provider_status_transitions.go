package db

import (
	"database/sql"
	"time"
)

// ProviderStatusTransition records an observed change in a cloud instance's
// provider-level status (e.g., "created" → "loading" → "running").
type ProviderStatusTransition struct {
	ID         int64
	LaunchID   int64
	ObservedAt int64  // unix timestamp
	OldStatus  string // "" for the first observation
	NewStatus  string
}

// RecordProviderStatus handles a provider status observation for a cloud instance.
// If oldStatus is non-empty (not the first observation), a transition row is inserted.
// If newStatus is "running", provider_running_at is set (first-write-wins).
func RecordProviderStatus(database *sql.DB, cloudInstanceID int64, observedAt time.Time, oldStatus, newStatus string) error {
	if oldStatus != "" {
		if _, err := database.Exec(
			`INSERT INTO provider_status_transitions (launch_id, observed_at, old_status, new_status) VALUES (?, ?, ?, ?)`,
			cloudInstanceID, observedAt.Unix(), oldStatus, newStatus,
		); err != nil {
			return err
		}
	}
	if newStatus == "running" {
		_ = SetLaunchProviderRunningAt(database, cloudInstanceID, observedAt)
	}
	return nil
}

// InsertProviderStatusTransition records a provider status change for a cloud instance.
// Deprecated: use RecordProviderStatus which also handles first-observation cases.
func InsertProviderStatusTransition(database *sql.DB, cloudInstanceID int64, observedAt time.Time, oldStatus, newStatus string) error {
	return RecordProviderStatus(database, cloudInstanceID, observedAt, oldStatus, newStatus)
}

// GetProviderStatusTransitions returns all recorded transitions for a cloud instance, ordered by time.
func GetProviderStatusTransitions(database *sql.DB, cloudInstanceID int64) ([]ProviderStatusTransition, error) {
	rows, err := database.Query(
		`SELECT id, launch_id, observed_at, old_status, new_status FROM provider_status_transitions WHERE launch_id = ? ORDER BY observed_at ASC`,
		cloudInstanceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var transitions []ProviderStatusTransition
	for rows.Next() {
		var t ProviderStatusTransition
		if err := rows.Scan(&t.ID, &t.LaunchID, &t.ObservedAt, &t.OldStatus, &t.NewStatus); err != nil {
			return nil, err
		}
		transitions = append(transitions, t)
	}
	return transitions, rows.Err()
}
