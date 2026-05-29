package db

import (
	"database/sql"
	"errors"
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
	// EventRelaunchSkippedWaitingOnProducer is emitted when a job's --needs
	// reference a producer that has not yet completed (or is on-prem with no
	// R2 copy). Skipping pre-flight avoids a guaranteed-fail launch attempt
	// against a fresh rental instance whose R2 lookup will 404.
	EventRelaunchSkippedWaitingOnProducer = "relaunch.skipped.waiting_on_producer"
	EventRelaunchSkippedBackoff           = "relaunch.skipped.backoff"
	EventRelaunchPassSummary              = "relaunch.pass_summary"
	EventRelaunchRunawayTripped           = "relaunch.runaway_tripped"
	EventRelaunchRunawayBlocked           = "relaunch.runaway_blocked"
	EventRelaunchRunawayResumed           = "relaunch.runaway_resumed"
	// EventRelaunchAutoProbeLaunched is emitted when the autopilot fires
	// a single probe launch while the runaway breaker is tripped. Detail
	// includes the probe launch_id so the auto-resume check can find it.
	EventRelaunchAutoProbeLaunched = "relaunch.auto_probe_launched"
	// EventRelaunchAutoProbeResumed is emitted when an auto-probe
	// instance completed successfully and the autopilot reset the
	// breaker as a result. Distinguishes auto-resume from manual reset.
	EventRelaunchAutoProbeResumed = "relaunch.auto_probe_resumed"

	// EventPlacementIntentPruned records that the autopilot canceled a stale
	// open placement intent (orchestrator died before resolving it). The
	// detail field carries the intent id, originating operation, and the
	// intent's age at prune time so leak patterns are diagnosable.
	EventPlacementIntentPruned = "placement_intent.pruned"

	// Launch progress (provider-specific bootstrap phases that can otherwise
	// look like an inert launching row).
	EventLaunchRunpodSSHWaiting = "launch.runpod_ssh_waiting"

	// Relaunch progress (same signal, scoped to the autopilot/relaunch status
	// line which watches relaunch.* events).
	EventRelaunchRunpodSSHWaiting = "relaunch.runpod_ssh_waiting"

	// Reconciliation actions (from ExecuteAction / reconcileStaleHeartbeat)
	EventReconcileBootstrapTimeout  = "reconcile.bootstrap_timeout"
	EventReconcileGraceExpired      = "reconcile.grace_expired"
	EventReconcileProviderDead      = "reconcile.provider_dead"
	EventReconcileStaleHeartbeat    = "reconcile.stale_heartbeat"
	EventReconcileSelfDestructFail  = "reconcile.self_destruct_failed"
	EventReconcileEmptyStatus       = "reconcile.empty_status_timeout"
	EventReconcileSafetyNetDestroy  = "reconcile.safety_net_destroy"
	EventReconcileOrphanSweep       = "reconcile.orphan_sweep"
	EventReconcileStalePlannedReap  = "reconcile.stale_planned_reap"
	EventReconcileDonorComplete     = "reconcile.donor_complete"
	EventReconcileTerminationIntent = "reconcile.termination_intent"
	EventReconcileBootstrapComplete = "reconcile.bootstrap_complete"
	EventReconcileSetupStall        = "reconcile.setup_stall"
	EventReconcileRunningStall      = "reconcile.running_stall"
	EventReconcileProviderPaused    = "reconcile.provider_paused"
	EventReconcileProviderResumed   = "reconcile.provider_resumed"
	EventReconcileHedgeCull         = "reconcile.hedge_cull"

	// Queue dispatch (host-sync push of queued jobs to remote queue runner).
	// EventQueueDispatchFailed records a per-job failure during
	// ensureQueuedJobsOnRemote (source sync, HF input staging, cloud artifact
	// staging, queue append, slurm submit). The detail field carries a short
	// "<stage>: <truncated err>" string suitable for surfacing as a job's
	// QueueBlockedReason. EventQueueDispatchOK clears prior failures by
	// providing a fresher floor for the hydrator query.
	EventQueueDispatchFailed = "queue.dispatch.failed"
	EventQueueDispatchOK     = "queue.dispatch.ok"
	// EventQueueDispatchDeferred records a *non-failure* skip during a
	// dispatch attempt — typically an ssh.IsConnectionError from a stage
	// the dispatcher returns early on. Without this, the latest visible
	// dispatch event remains the most recent .failed for hours after a
	// transient host outage masked itself as the active blocker.
	// dispatchBlockedReasonsFromEvents treats a fresher .deferred as
	// superseding an older .failed.
	EventQueueDispatchDeferred = "queue.dispatch.deferred"
	// EventQueueDispatchAutoReplanned records an automatic move from an
	// inventory-host queue back to the unplaced pool after sustained dispatch
	// failures made the current target a poor placement.
	EventQueueDispatchAutoReplanned = "queue.dispatch.auto_replanned"

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

// InsertLifecycleEventDedup inserts a lifecycle event unless an event
// with the same (event_kind, job_id, detail) has already been recorded
// within the freshness window. Used by retry-prone dispatch sites
// (recordFailure, recordDeferred) to keep the audit log from filling
// with byte-identical rows when the same failure recurs every tick.
//
// Best-effort: a concurrent caller seeing "no recent row" simultaneously
// can still cause two inserts. The hydrator's latest-wins semantics
// tolerate that — the dedupe is a noise reducer, not a correctness
// requirement. Returns whether the row was inserted (true) or skipped
// as a duplicate (false).
func InsertLifecycleEventDedup(database *sql.DB, event *LifecycleEvent, freshness time.Duration) (bool, error) {
	if database == nil {
		return false, nil
	}
	if freshness <= 0 || event.JobID == 0 {
		return true, InsertLifecycleEvent(database, event)
	}
	cutoff := time.Now().Add(-freshness).Unix()
	var existing string
	err := database.QueryRow(`
		SELECT COALESCE(detail, '')
		FROM lifecycle_events
		WHERE event_kind = ?
		  AND job_id = ?
		  AND occurred_at > ?
		ORDER BY id DESC LIMIT 1`,
		event.EventKind, event.JobID, cutoff,
	).Scan(&existing)
	switch {
	case err == nil:
		if existing == event.Detail {
			return false, nil
		}
	case errors.Is(err, sql.ErrNoRows):
		// No recent same-kind event for this job — proceed to insert.
	default:
		return false, err
	}
	return true, InsertLifecycleEvent(database, event)
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

// LatestLifecycleEventID returns the highest lifecycle event id currently in
// the database. It is suitable as a cursor for tailing newly inserted events.
func LatestLifecycleEventID(database *sql.DB) (int64, error) {
	if database == nil {
		return 0, nil
	}
	var id sql.NullInt64
	if err := database.QueryRow(`SELECT MAX(id) FROM lifecycle_events`).Scan(&id); err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// ListLifecycleEventsAfterID returns events with id greater than afterID,
// ordered from oldest to newest. This is the streaming/tailing counterpart to
// ListLifecycleEvents, whose newest-first order is better for inspection UIs.
func ListLifecycleEventsAfterID(database *sql.DB, afterID int64, limit int) ([]LifecycleEvent, error) {
	if database == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := database.Query(`
		SELECT id, occurred_at, event_kind,
			COALESCE(launch_id, 0), COALESCE(campaign_id, 0), COALESCE(job_id, 0),
			COALESCE(gpu_spec, ''), COALESCE(job_count, 0),
			COALESCE(detail, ''), COALESCE(error_text, ''),
			COALESCE(attempt_number, 0), COALESCE(max_attempts, 0), COALESCE(disk_gb, 0)
		FROM lifecycle_events
		WHERE id > ?
		ORDER BY id ASC
		LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLifecycleEvents(rows)
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

	return scanLifecycleEvents(rows)
}

func scanLifecycleEvents(rows *sql.Rows) ([]LifecycleEvent, error) {
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
