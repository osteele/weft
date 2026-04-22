package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Event kind constants for lifecycle_events table.
// Naming convention: <subsystem>.<action>[.<detail>]
const (
	// Relaunch decisions (from RelaunchOrphanedJobs)
	EventRelaunchEligible           = "relaunch.eligible"
	EventRelaunchSkippedMaxAttempts = "relaunch.skipped.max_attempts"
	EventRelaunchSkippedNoOffers    = "relaunch.skipped.no_offers"
	EventRelaunchSkippedOfferError  = "relaunch.skipped.offer_error"
	EventRelaunchDiskBump           = "relaunch.disk_bump"
	EventRelaunchLaunchSuccess      = "relaunch.launch_success"
	EventRelaunchLaunchFailed       = "relaunch.launch_failed"
	EventRelaunchSkippedNoClient    = "relaunch.skipped.no_client"
	EventRelaunchPassSummary        = "relaunch.pass_summary"
	EventRelaunchRunawayTripped     = "relaunch.runaway_tripped"
	EventRelaunchRunawayBlocked     = "relaunch.runaway_blocked"
	EventRelaunchRunawayResumed     = "relaunch.runaway_resumed"

	// Reconciliation actions (from ExecuteAction / reconcileStaleHeartbeat)
	EventReconcileBootstrapTimeout  = "reconcile.bootstrap_timeout"
	EventReconcileGraceExpired      = "reconcile.grace_expired"
	EventReconcileProviderDead      = "reconcile.provider_dead"
	EventReconcileStaleHeartbeat    = "reconcile.stale_heartbeat"
	EventReconcileSelfDestructFail  = "reconcile.self_destruct_failed"
	EventReconcileEmptyStatus       = "reconcile.empty_status_timeout"
	EventReconcileSafetyNetDestroy  = "reconcile.safety_net_destroy"
	EventReconcileOrphanSweep       = "reconcile.orphan_sweep"
	EventReconcileDonorComplete     = "reconcile.donor_complete"
	EventReconcileTerminationIntent = "reconcile.termination_intent"
	EventReconcileBootstrapComplete = "reconcile.bootstrap_complete"
	EventReconcileSetupStall        = "reconcile.setup_stall"
	EventReconcileRunningStall      = "reconcile.running_stall"

	// TUI retry outcomes
	EventRetryAutoTriggered   = "retry.auto_triggered"
	EventRetryManualTriggered = "retry.manual_triggered"
	EventRetrySuccess         = "retry.success"
	EventRetryNoOffers        = "retry.no_offers"
	EventRetryExhausted       = "retry.exhausted"
	EventRetryMaxAttempts     = "retry.max_attempts"
	EventRetryError           = "retry.error"
)

// LifecycleEvent records a structured decision or state transition in the
// instance/job lifecycle. Most fields are optional (zero value = omitted).
type LifecycleEvent struct {
	ID            int64
	OccurredAt    int64  // unix timestamp
	EventKind     string // one of the Event* constants
	LaunchID      int64  // 0 = not applicable
	CampaignID    int64  // 0 = not applicable
	JobID         int64  // 0 = not applicable
	GPUSpec       string
	JobCount      int
	Detail        string // short structured note
	ErrorText     string // error message if applicable
	AttemptNumber int    // which relaunch attempt
	MaxAttempts   int    // configured max at decision time
	DiskGB        int    // disk allocation (for disk bump events)
}

// InsertLifecycleEvent writes a lifecycle event to the database.
func InsertLifecycleEvent(database *sql.DB, event *LifecycleEvent) error {
	if database == nil {
		return nil
	}
	occurredAt := event.OccurredAt
	if occurredAt == 0 {
		occurredAt = time.Now().Unix()
	}
	_, err := database.Exec(`
		INSERT INTO lifecycle_events
			(occurred_at, event_kind, launch_id, campaign_id, job_id, gpu_spec,
			 job_count, detail, error_text, attempt_number, max_attempts, disk_gb)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		occurredAt,
		event.EventKind,
		nullIfZero(event.LaunchID),
		nullIfZero(event.CampaignID),
		nullIfZero(event.JobID),
		nullIfEmpty(event.GPUSpec),
		nullIfZero(int64(event.JobCount)),
		nullIfEmpty(event.Detail),
		nullIfEmpty(event.ErrorText),
		nullIfZero(int64(event.AttemptNumber)),
		nullIfZero(int64(event.MaxAttempts)),
		nullIfZero(int64(event.DiskGB)),
	)
	return err
}

// LifecycleEventFilter controls which events are returned by ListLifecycleEvents.
type LifecycleEventFilter struct {
	KindPrefix string    // e.g. "relaunch." matches all relaunch events
	Kind       string    // exact match
	LaunchID   int64     // 0 = no filter
	CampaignID int64     // 0 = no filter
	Since      time.Time // zero = no lower bound
	ErrorsOnly bool
	Limit      int // 0 = default 200
}

// ListLifecycleEvents returns events matching the filter, ordered by occurred_at DESC.
func ListLifecycleEvents(database *sql.DB, filter LifecycleEventFilter) ([]LifecycleEvent, error) {
	var conditions []string
	var args []any

	if filter.Kind != "" {
		conditions = append(conditions, "event_kind = ?")
		args = append(args, filter.Kind)
	} else if filter.KindPrefix != "" {
		conditions = append(conditions, "event_kind LIKE ?")
		args = append(args, filter.KindPrefix+"%")
	}

	if filter.LaunchID != 0 {
		conditions = append(conditions, "launch_id = ?")
		args = append(args, filter.LaunchID)
	}

	if filter.CampaignID != 0 {
		conditions = append(conditions, "campaign_id = ?")
		args = append(args, filter.CampaignID)
	}

	if !filter.Since.IsZero() {
		conditions = append(conditions, "occurred_at >= ?")
		args = append(args, filter.Since.Unix())
	}

	if filter.ErrorsOnly {
		conditions = append(conditions, "error_text IS NOT NULL AND error_text != ''")
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}

	query := fmt.Sprintf(`
		SELECT id, occurred_at, event_kind,
			COALESCE(launch_id, 0), COALESCE(campaign_id, 0), COALESCE(job_id, 0),
			COALESCE(gpu_spec, ''), COALESCE(job_count, 0),
			COALESCE(detail, ''), COALESCE(error_text, ''),
			COALESCE(attempt_number, 0), COALESCE(max_attempts, 0), COALESCE(disk_gb, 0)
		FROM lifecycle_events%s
		ORDER BY occurred_at DESC
		LIMIT ?`, where)
	args = append(args, limit)

	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []LifecycleEvent
	for rows.Next() {
		var e LifecycleEvent
		if err := rows.Scan(
			&e.ID, &e.OccurredAt, &e.EventKind,
			&e.LaunchID, &e.CampaignID, &e.JobID,
			&e.GPUSpec, &e.JobCount,
			&e.Detail, &e.ErrorText,
			&e.AttemptNumber, &e.MaxAttempts, &e.DiskGB,
		); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// LatestLifecycleEvent returns the most recent event matching the filter, or
// nil if none found. Convenience wrapper over ListLifecycleEvents with
// Limit=1. Intended for "what is the autopilot doing right now?" queries
// where only the most recent event matters.
func LatestLifecycleEvent(database *sql.DB, filter LifecycleEventFilter) (*LifecycleEvent, error) {
	filter.Limit = 1
	events, err := ListLifecycleEvents(database, filter)
	if err != nil || len(events) == 0 {
		return nil, err
	}
	return &events[0], nil
}

// LifecycleEventKindCount holds a count for a single event kind.
type LifecycleEventKindCount struct {
	Kind  string
	Count int
}

// CountLifecycleEventsByKind returns event counts grouped by kind, ordered by count descending.
func CountLifecycleEventsByKind(database *sql.DB, filter LifecycleEventFilter) ([]LifecycleEventKindCount, error) {
	var conditions []string
	var args []any

	if filter.Kind != "" {
		conditions = append(conditions, "event_kind = ?")
		args = append(args, filter.Kind)
	} else if filter.KindPrefix != "" {
		conditions = append(conditions, "event_kind LIKE ?")
		args = append(args, filter.KindPrefix+"%")
	}

	if filter.LaunchID != 0 {
		conditions = append(conditions, "launch_id = ?")
		args = append(args, filter.LaunchID)
	}

	if !filter.Since.IsZero() {
		conditions = append(conditions, "occurred_at >= ?")
		args = append(args, filter.Since.Unix())
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT event_kind, COUNT(*) as cnt
		FROM lifecycle_events%s
		GROUP BY event_kind
		ORDER BY cnt DESC`, where)

	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var counts []LifecycleEventKindCount
	for rows.Next() {
		var kc LifecycleEventKindCount
		if err := rows.Scan(&kc.Kind, &kc.Count); err != nil {
			return nil, err
		}
		counts = append(counts, kc)
	}
	return counts, rows.Err()
}
