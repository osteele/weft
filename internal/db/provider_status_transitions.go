package db

import (
	"database/sql"
	"errors"
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

// RecordProviderStatus records a provider status observation for a cloud
// instance, inserting a row when the status differs from the last one on
// record.
//
// The empty→non-empty edge is recorded deliberately. The empty-status watchdog
// waits for the provider to report any status at all and terminates if that
// takes too long, so the moment a status first becomes legible is the event
// that window is measured against. Without the row the threshold cannot be
// derived from history, and the rule cannot derive it itself: it terminates
// instances before they produce the observation.
//
// A launch therefore carries exactly one empty-oldStatus row, written when its
// provider first became legible, and that row's observed_at minus launched_at
// is the latency the watchdog governs.
//
// If newStatus is "running", provider_running_at is set (first-write-wins).
func RecordProviderStatus(database *sql.DB, cloudInstanceID int64, observedAt time.Time, oldStatus, newStatus string) error {
	// An empty oldStatus means the *caller* holds no previous status, which is
	// not the same as the launch having none. Reconcilers are constructed per
	// poll in several paths (cmd/status.go builds one inside a 3s ticker), so
	// a caller with fresh memory reports "" on every tick. Fall back to the
	// table, which outlives any one observer: it yields the real predecessor
	// across an observer restart, and suppresses the repeats that would
	// otherwise accumulate a row per tick and make a launch's first
	// observation look hours late.
	if oldStatus == "" {
		prior, err := lastRecordedProviderStatus(database, cloudInstanceID)
		if err != nil {
			return err
		}
		if prior != nil && *prior == newStatus {
			if newStatus == "running" {
				_ = SetLaunchProviderRunningAt(database, cloudInstanceID, observedAt)
			}
			return nil
		}
		if prior != nil {
			oldStatus = *prior
		}
	}
	if _, err := database.Exec(
		`INSERT INTO provider_status_transitions (launch_id, observed_at, old_status, new_status) VALUES (?, ?, ?, ?)`,
		cloudInstanceID, observedAt.Unix(), oldStatus, newStatus,
	); err != nil {
		return err
	}
	if newStatus == "running" {
		_ = SetLaunchProviderRunningAt(database, cloudInstanceID, observedAt)
	}
	return nil
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
// recorded provider-status change for a launch, or nil when none exist. Used
// by reconciliation to anchor stale-status timeouts on the time the status
// went non-running rather than on the original launch time.
//
// Rows with an empty old_status are excluded: they record that the provider
// became legible, not that its status changed, and anchoring on one would push
// the deadline out by the provider's own reporting latency.
func LastProviderStatusTransitionTime(database *sql.DB, launchID int64) (*time.Time, error) {
	var observedAt sql.NullInt64
	err := database.QueryRow(
		`SELECT MAX(observed_at) FROM provider_status_transitions WHERE launch_id = ? AND old_status != ''`,
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

// lastRecordedProviderStatus returns the most recently recorded new_status for
// a launch, or nil when the launch has no transitions yet. Nil means "no row",
// never "the read failed" — an error is returned as an error, so a caller
// cannot mistake a failed lookup for a first observation and write a spurious
// one.
func lastRecordedProviderStatus(database *sql.DB, launchID int64) (*string, error) {
	var status sql.NullString
	err := database.QueryRow(
		`SELECT new_status FROM provider_status_transitions WHERE launch_id = ? ORDER BY observed_at DESC, id DESC LIMIT 1`,
		launchID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !status.Valid {
		return nil, nil
	}
	s := status.String
	return &s, nil
}
