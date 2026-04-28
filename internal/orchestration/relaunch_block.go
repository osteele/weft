package orchestration

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/queueblock"
)

// HydrateUnplacedBlockedReasons populates QueueBlockedReason on unplaced
// jobs from both fresh relaunch lifecycle events and on-demand producer
// state. Pairs the two existing hydration steps so callers only need one.
func HydrateUnplacedBlockedReasons(database *sql.DB, jobs []*db.Job) {
	HydrateRelaunchBlockedReasons(database, jobs)
	queueblock.HydrateWaitingOnProducerReasons(database, jobs)
	HydrateInventoryDispatchBlockedReasons(database, jobs)
}

// HydrateInventoryDispatchBlockedReasons populates QueueBlockedReason on
// inventory-host queued jobs whose remote dispatch has been failing
// (source-sync rsync error, HF input staging failure, queue append failure,
// etc.). Such failures are otherwise invisible in weft's UI: the job appears
// as a normal "queued" job indefinitely while ensureQueuedJobsOnRemote logs
// only to operations.log.
//
// Reads queue.dispatch.failed events for each candidate job, ignoring events
// older than the most recent queue.dispatch.ok (so a successful dispatch
// clears stale failure reasons).
func HydrateInventoryDispatchBlockedReasons(database *sql.DB, jobs []*db.Job) {
	if database == nil || len(jobs) == 0 {
		return
	}
	floorByJob := make(map[int64]int64, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if !job.HasInventoryHost() || job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		if strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		floor := job.QueuedAt
		if floor <= 0 {
			floor = job.CreatedAt
		}
		floorByJob[job.ID] = floor
	}
	if len(floorByJob) == 0 {
		return
	}
	reasons := dispatchBlockedReasonsFromEvents(database, floorByJob)
	if len(reasons) == 0 {
		return
	}
	for _, job := range jobs {
		if job == nil || strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		if reason := strings.TrimSpace(reasons[job.ID]); reason != "" {
			job.QueueBlockedReason = reason
		}
	}
}

// dispatchBlockedReasonsFromEvents returns the latest queue.dispatch.failed
// detail per job, suppressed if a queue.dispatch.ok event exists at the same
// or later timestamp (the failure has been resolved on a subsequent pass).
func dispatchBlockedReasonsFromEvents(database *sql.DB, floorByJob map[int64]int64) map[int64]string {
	reasons := make(map[int64]string)
	if database == nil || len(floorByJob) == 0 {
		return reasons
	}
	jobIDs := make([]int64, 0, len(floorByJob))
	for jobID := range floorByJob {
		jobIDs = append(jobIDs, jobID)
	}
	placeholders := make([]string, 0, len(jobIDs))
	args := make([]any, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		placeholders = append(placeholders, "?")
		args = append(args, jobID)
	}
	query := fmt.Sprintf(`SELECT job_id, occurred_at, event_kind, COALESCE(detail, '')
		FROM lifecycle_events
		WHERE event_kind IN (?, ?)
		  AND job_id IN (%s)
		ORDER BY occurred_at DESC, id DESC`, strings.Join(placeholders, ","))
	queryArgs := append([]any{db.EventQueueDispatchFailed, db.EventQueueDispatchOK}, args...)
	rows, err := database.Query(query, queryArgs...)
	if err != nil {
		return reasons
	}
	defer rows.Close()

	latestOK := make(map[int64]int64)
	for rows.Next() {
		var (
			jobID      int64
			occurredAt int64
			kind       string
			detail     string
		)
		if err := rows.Scan(&jobID, &occurredAt, &kind, &detail); err != nil {
			continue
		}
		if floor, ok := floorByJob[jobID]; ok && floor > 0 && occurredAt < floor {
			continue
		}
		switch kind {
		case db.EventQueueDispatchOK:
			if existing, ok := latestOK[jobID]; !ok || occurredAt > existing {
				latestOK[jobID] = occurredAt
			}
		case db.EventQueueDispatchFailed:
			if _, exists := reasons[jobID]; exists {
				continue
			}
			if okAt := latestOK[jobID]; okAt > 0 && okAt >= occurredAt {
				continue
			}
			detail = strings.TrimSpace(detail)
			if detail == "" {
				continue
			}
			reasons[jobID] = detail
		}
	}
	return reasons
}

// relaunchOfferErrorFreshness bounds how long a relaunch.skipped.offer_error
// event remains a visible block reason. Offer-error events come from transient
// provider API failures (e.g. vastai SSL flakes); a successful subsequent poll
// does not emit a companion "cleared" event, so an old error would otherwise
// stick as the job's blocked reason forever. Any skip event older than this
// window is treated as stale and ignored, letting a fresher event (or silence)
// take over. Other skip kinds (max_attempts, no_offers, no_client) reflect
// durable conditions and are not subject to this window.
const relaunchOfferErrorFreshness = 5 * time.Minute

func BuildFailedInstanceByJob(database *sql.DB, jobIDs []int64) map[int64]int64 {
	result := make(map[int64]int64, len(jobIDs))
	for _, jobID := range jobIDs {
		result[jobID] = LatestAttemptLaunchID(database, jobID)
	}
	return result
}

func LatestAttemptLaunchID(database *sql.DB, jobID int64) int64 {
	attempts, err := db.GetLaunchAttempts(database, jobID)
	if err != nil || len(attempts) == 0 {
		return 0
	}
	return attempts[len(attempts)-1].LaunchID
}

func HydrateRelaunchBlockedReasons(database *sql.DB, jobs []*db.Job) {
	if database == nil || len(jobs) == 0 {
		return
	}
	queueFloorByJob := make(map[int64]int64, len(jobs))
	for _, job := range jobs {
		if !job.IsUnplacedAwaitingPlacement() {
			continue
		}
		if strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		floor := job.QueuedAt
		if floor <= 0 {
			floor = job.CreatedAt
		}
		queueFloorByJob[job.ID] = floor
	}
	if len(queueFloorByJob) == 0 {
		return
	}
	reasons := RelaunchBlockedReasonsFromEventsWithFloor(database, queueFloorByJob)
	if len(reasons) == 0 {
		return
	}
	for _, job := range jobs {
		if job == nil || strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		if reason := strings.TrimSpace(reasons[job.ID]); reason != "" {
			job.QueueBlockedReason = reason
		}
	}
}

func RelaunchBlockedReasonsFromEventsWithFloor(database *sql.DB, floorByJob map[int64]int64) map[int64]string {
	reasons := make(map[int64]string)
	if database == nil || len(floorByJob) == 0 {
		return reasons
	}
	jobIDs := make([]int64, 0, len(floorByJob))
	for jobID := range floorByJob {
		jobIDs = append(jobIDs, jobID)
	}
	placeholders := make([]string, 0, len(jobIDs))
	args := make([]any, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		placeholders = append(placeholders, "?")
		args = append(args, jobID)
	}
	query := fmt.Sprintf(`SELECT job_id, occurred_at, event_kind, detail, attempt_number, max_attempts
		FROM lifecycle_events
		WHERE event_kind LIKE 'relaunch.skipped.%%'
		  AND job_id IN (%s)
		ORDER BY occurred_at DESC, id DESC`, strings.Join(placeholders, ","))
	rows, err := database.Query(query, args...)
	if err != nil {
		return reasons
	}
	defer rows.Close()
	for rows.Next() {
		var (
			jobID         int64
			occurredAt    int64
			eventKind     string
			detail        sql.NullString
			attemptNumber sql.NullInt64
			maxAttempts   sql.NullInt64
		)
		if err := rows.Scan(&jobID, &occurredAt, &eventKind, &detail, &attemptNumber, &maxAttempts); err != nil {
			continue
		}
		if _, exists := reasons[jobID]; exists {
			continue
		}
		if floor, ok := floorByJob[jobID]; ok && floor > 0 && occurredAt < floor {
			continue
		}
		if eventKind == db.EventRelaunchSkippedOfferError && isRelaunchOfferErrorStale(occurredAt) {
			continue
		}
		reasons[jobID] = summarizeRelaunchSkipEvent(eventKind, detail.String, int(attemptNumber.Int64), int(maxAttempts.Int64))
	}
	return reasons
}

func isRelaunchOfferErrorStale(occurredAt int64) bool {
	if occurredAt <= 0 {
		return false
	}
	return time.Since(time.Unix(occurredAt, 0)) > relaunchOfferErrorFreshness
}

func summarizeRelaunchSkipEvent(kind, detail string, attemptNumber, maxAttempts int) string {
	detail = strings.TrimSpace(detail)
	if detail != "" {
		return detail
	}
	switch kind {
	case db.EventRelaunchSkippedNoOffers:
		return "no offers available"
	case db.EventRelaunchSkippedOfferError:
		return "offer query failed"
	case db.EventRelaunchSkippedMaxAttempts:
		if attemptNumber > 0 && maxAttempts > 0 {
			return "attempt " + strconv.Itoa(attemptNumber) + "/" + strconv.Itoa(maxAttempts)
		}
		return "max cloud attempts reached"
	default:
		return kind
	}
}
