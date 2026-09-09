package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const JobEventSchemaVersion = "weft-job-event/v1"

// JobLifecycleEvent is the durable, versioned fact delivered to lifecycle hooks.
type JobLifecycleEvent struct {
	SchemaVersion    string `json:"schema_version"`
	EventID          string `json:"event_id"`
	JobID            string `json:"job_id"`
	JobNumericID     int64  `json:"-"`
	EventSequence    int64  `json:"event_sequence"`
	AttemptID        int64  `json:"attempt_id"`
	AttemptNumber    int    `json:"attempt_number"`
	EventKind        string `json:"event_kind"`
	Status           string `json:"status"`
	OccurredAt       int64  `json:"occurred_at"`
	Project          string `json:"project"`
	ProjectRoot      string `json:"project_root"`
	SubmitterSession string `json:"submitter_session"`
}

// EnsureJobTerminalEvent backfills the trigger-owned event when a legacy
// caller invokes the terminal notification seam without performing the status
// transition itself. The unique index makes this a no-op on normal paths.
func EnsureJobTerminalEvent(database *sql.DB, jobID int64, status string, occurredAt time.Time) error {
	if database == nil {
		return fmt.Errorf("ensure terminal job event: nil database")
	}
	_, err := database.Exec(`
		INSERT OR IGNORE INTO job_lifecycle_events (
			event_id, job_id, event_sequence, attempt_id, attempt_number,
			event_kind, status, occurred_at, project, project_root, submitter_session
		)
		SELECT ?, j.id,
		       (SELECT COALESCE(MAX(event_sequence), 0) + 1
		          FROM job_lifecycle_events WHERE job_id = j.id),
		       a.id, COALESCE(a.attempt_number, 0), 'job.terminal', ?, ?,
		       COALESCE(j.project, ''), COALESCE(j.project_root, ''),
		       COALESCE(j.submitter_session, '')
		FROM jobs AS j
		LEFT JOIN authoritative_job_attempts AS a
		  ON a.id = (SELECT a2.id FROM authoritative_job_attempts AS a2
		             WHERE a2.job_id = j.id
		             ORDER BY a2.attempt_number DESC LIMIT 1)
		WHERE j.id = ?`,
		uuid.NewString(), status, occurredAt.Unix(), jobID)
	if err != nil {
		return fmt.Errorf("ensure terminal job event for job %d: %w", jobID, err)
	}
	return nil
}

// LifecycleHookDelivery is one leased delivery. LeaseToken fences completion
// writes from a worker whose lease expired and was reclaimed.
type LifecycleHookDelivery struct {
	Event      JobLifecycleEvent
	HookID     string
	LeaseToken string
	Attempts   int
}

// ClaimLifecycleHookDeliveries registers hookID on first sight, materializes
// its delivery rows for events emitted after registration plus the focused job,
// and leases due work. claimJobID = 0 claims every due event.
func ClaimLifecycleHookDeliveries(database *sql.DB, hookID string, materializeJobID, claimJobID int64, now time.Time, lease time.Duration, limit int) ([]LifecycleHookDelivery, error) {
	if database == nil {
		return nil, fmt.Errorf("claim lifecycle hook deliveries: nil database")
	}
	if hookID == "" {
		return nil, fmt.Errorf("claim lifecycle hook deliveries: empty hook id")
	}
	if limit <= 0 {
		return nil, nil
	}
	nowUnix := now.Unix()
	tx, err := database.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin lifecycle hook claim: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO lifecycle_hook_registrations (hook_id, registered_at)
		VALUES (?, ?)
		ON CONFLICT(hook_id) DO NOTHING`, hookID, nowUnix); err != nil {
		return nil, fmt.Errorf("register lifecycle hook %q: %w", hookID, err)
	}
	if _, err := tx.Exec(`
		INSERT INTO lifecycle_hook_deliveries
			(event_id, hook_id, state, attempts, next_attempt_at, updated_at)
		SELECT e.event_id, ?, 'pending', 0, 0, ?
		FROM job_lifecycle_events AS e
		JOIN lifecycle_hook_registrations AS r ON r.hook_id = ?
		WHERE e.occurred_at >= r.registered_at
		   OR (? != 0 AND e.job_id = ?)
		ON CONFLICT(event_id, hook_id) DO NOTHING`,
		hookID, nowUnix, hookID, materializeJobID, materializeJobID); err != nil {
		return nil, fmt.Errorf("materialize lifecycle deliveries for %q: %w", hookID, err)
	}

	rows, err := tx.Query(`
		SELECT d.event_id
		FROM lifecycle_hook_deliveries AS d
		JOIN job_lifecycle_events AS e ON e.event_id = d.event_id
		WHERE d.hook_id = ? AND d.state = 'pending' AND d.next_attempt_at <= ?
		  AND (d.lease_until IS NULL OR d.lease_until <= ?)
		  AND (? = 0 OR e.job_id = ?)
		ORDER BY e.job_id, e.event_sequence
		LIMIT ?`, hookID, nowUnix, nowUnix, claimJobID, claimJobID, limit)
	if err != nil {
		return nil, fmt.Errorf("select lifecycle deliveries for %q: %w", hookID, err)
	}
	var eventIDs []string
	for rows.Next() {
		var eventID string
		if err := rows.Scan(&eventID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan lifecycle delivery id: %w", err)
		}
		eventIDs = append(eventIDs, eventID)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close lifecycle delivery ids: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate lifecycle delivery ids: %w", err)
	}

	claimed := make([]LifecycleHookDelivery, 0, len(eventIDs))
	leaseUntil := now.Add(lease).Unix()
	for _, eventID := range eventIDs {
		token := uuid.NewString()
		result, err := tx.Exec(`
			UPDATE lifecycle_hook_deliveries
			SET lease_token = ?, lease_until = ?, attempts = attempts + 1, updated_at = ?
			WHERE event_id = ? AND hook_id = ? AND state = 'pending'
			  AND next_attempt_at <= ? AND (lease_until IS NULL OR lease_until <= ?)`,
			token, leaseUntil, nowUnix, eventID, hookID, nowUnix, nowUnix)
		if err != nil {
			return nil, fmt.Errorf("lease lifecycle delivery %s/%s: %w", eventID, hookID, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count lifecycle delivery lease %s/%s: %w", eventID, hookID, err)
		}
		if count != 1 {
			continue
		}
		var delivery LifecycleHookDelivery
		delivery.HookID = hookID
		delivery.LeaseToken = token
		delivery.Event.SchemaVersion = JobEventSchemaVersion
		if err := tx.QueryRow(`
			SELECT e.event_id, e.job_id, e.event_sequence, COALESCE(e.attempt_id, 0),
			       e.attempt_number, e.event_kind, e.status, e.occurred_at,
			       e.project, e.project_root, e.submitter_session, d.attempts
			FROM job_lifecycle_events AS e
			JOIN lifecycle_hook_deliveries AS d ON d.event_id = e.event_id
			WHERE e.event_id = ? AND d.hook_id = ?`, eventID, hookID).Scan(
			&delivery.Event.EventID,
			&delivery.Event.JobNumericID,
			&delivery.Event.EventSequence,
			&delivery.Event.AttemptID,
			&delivery.Event.AttemptNumber,
			&delivery.Event.EventKind,
			&delivery.Event.Status,
			&delivery.Event.OccurredAt,
			&delivery.Event.Project,
			&delivery.Event.ProjectRoot,
			&delivery.Event.SubmitterSession,
			&delivery.Attempts,
		); err != nil {
			return nil, fmt.Errorf("load lifecycle delivery %s/%s: %w", eventID, hookID, err)
		}
		delivery.Event.JobID = fmt.Sprintf("wj%d", delivery.Event.JobNumericID)
		claimed = append(claimed, delivery)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit lifecycle hook claims: %w", err)
	}
	return claimed, nil
}

// FinishLifecycleHookDelivery records an acknowledgement from the worker that
// owns leaseToken. A stale worker cannot overwrite a later claimant's result.
func FinishLifecycleHookDelivery(database *sql.DB, eventID, hookID, leaseToken, disposition, detail string, now time.Time, retryAfter time.Duration) (bool, error) {
	state := "delivered"
	deliveredAt := any(now.Unix())
	nextAttemptAt := int64(0)
	switch disposition {
	case "handled", "ignored":
	case "rejected":
		state = "rejected"
	case "retry":
		state = "pending"
		deliveredAt = nil
		nextAttemptAt = now.Add(retryAfter).Unix()
	default:
		return false, fmt.Errorf("finish lifecycle hook delivery: invalid disposition %q", disposition)
	}
	result, err := database.Exec(`
		UPDATE lifecycle_hook_deliveries
		SET state = ?, disposition = ?, next_attempt_at = ?, lease_token = NULL,
		    lease_until = NULL, last_error = NULLIF(?, ''), delivered_at = ?, updated_at = ?
		WHERE event_id = ? AND hook_id = ? AND state = 'pending' AND lease_token = ?`,
		state, disposition, nextAttemptAt, detail, deliveredAt, now.Unix(), eventID, hookID, leaseToken)
	if err != nil {
		return false, fmt.Errorf("finish lifecycle hook delivery %s/%s: %w", eventID, hookID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count finished lifecycle delivery %s/%s: %w", eventID, hookID, err)
	}
	return count == 1, nil
}

// RecordLifecycleHookRetry releases a claimed delivery after an execution or
// protocol failure. The error stays inspectable while the event remains due.
func RecordLifecycleHookRetry(database *sql.DB, delivery LifecycleHookDelivery, detail string, now time.Time, retryAfter time.Duration) (bool, error) {
	result, err := database.Exec(`
		UPDATE lifecycle_hook_deliveries
		SET next_attempt_at = ?, lease_token = NULL, lease_until = NULL,
		    last_error = ?, updated_at = ?
		WHERE event_id = ? AND hook_id = ? AND state = 'pending' AND lease_token = ?`,
		now.Add(retryAfter).Unix(), detail, now.Unix(), delivery.Event.EventID, delivery.HookID, delivery.LeaseToken)
	if err != nil {
		return false, fmt.Errorf("retry lifecycle hook delivery %s/%s: %w", delivery.Event.EventID, delivery.HookID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count retried lifecycle delivery %s/%s: %w", delivery.Event.EventID, delivery.HookID, err)
	}
	return count == 1, nil
}
