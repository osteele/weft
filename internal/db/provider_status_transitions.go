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

// LaunchPausedSeconds returns the total number of seconds the launch's
// provider instance spent in the "stopped" state (typical of a Vast.ai
// interruptible bid loss). Open stop intervals — where the instance is
// stopped now and we haven't yet observed a resume — are extended to
// referenceTime. The result is wall-clock minus runtime for analytics that
// want to attribute pause time separately. Returns 0 with no error when
// the launch has no recorded transitions.
func LaunchPausedSeconds(database *sql.DB, cloudInstanceID int64, referenceTime time.Time) (int64, error) {
	transitions, err := GetProviderStatusTransitions(database, cloudInstanceID)
	if err != nil {
		return 0, err
	}
	const stopped = "stopped"
	var total int64
	var stopStart int64
	inStop := false
	for _, t := range transitions {
		if t.NewStatus == stopped && !inStop {
			stopStart = t.ObservedAt
			inStop = true
			continue
		}
		if t.NewStatus != stopped && inStop {
			total += t.ObservedAt - stopStart
			inStop = false
		}
	}
	if inStop {
		total += referenceTime.Unix() - stopStart
	}
	if total < 0 {
		return 0, nil
	}
	return total, nil
}

// LastProviderStatusTransitionTime returns the time of the most recent
// recorded provider-status transition for a launch, or nil when no
// transitions exist. Used by reconciliation to anchor stale-status
// timeouts on the time the status went non-running rather than on the
// original launch time.
func LastProviderStatusTransitionTime(database *sql.DB, launchID int64) (*time.Time, error) {
	var observedAt sql.NullInt64
	err := database.QueryRow(
		`SELECT MAX(observed_at) FROM provider_status_transitions WHERE launch_id = ?`,
		launchID,
	).Scan(&observedAt)
	if err != nil {
		return nil, err
	}
	if !observedAt.Valid {
		return nil, nil
	}
	t := time.Unix(observedAt.Int64, 0)
	return &t, nil
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
