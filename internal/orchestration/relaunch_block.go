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
	HydrateCloudQueuedRetryBlockedReasons(database, jobs)
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

// HydrateCloudQueuedRetryBlockedReasons surfaces retry blockers for queued
// cloud jobs that were restored to an existing launch after a no-start
// new-instance attempt. These jobs are no longer unplaced, so the normal
// unplaced blocked-reason hydration does not see them, but from the user's
// perspective the manual "new instance" action is still blocked.
func HydrateCloudQueuedRetryBlockedReasons(database *sql.DB, jobs []*db.Job) {
	if database == nil || len(jobs) == 0 {
		return
	}
	candidates := make(map[int64]*db.Job, len(jobs))
	for _, job := range jobs {
		if job == nil || strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		if job.EffectiveStatus() != db.StatusQueued || job.TargetKind() != db.JobTargetRentalInstance {
			continue
		}
		candidates[job.ID] = job
	}
	if len(candidates) == 0 {
		return
	}
	floorByJob := cloudQueuedRetryFloorByJob(database, candidates)
	if len(floorByJob) == 0 {
		return
	}
	reasons := RelaunchBlockedReasonsFromEventsWithFloor(database, floorByJob)
	for jobID, reason := range relaunchRunawayBlockedReasonsWithFloor(database, floorByJob) {
		if _, exists := reasons[jobID]; !exists {
			reasons[jobID] = reason
		}
	}
	if len(reasons) == 0 {
		return
	}
	for jobID, reason := range reasons {
		job := candidates[jobID]
		if job == nil || strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		if strings.TrimSpace(reason) != "" {
			job.QueueBlockedReason = reason
		}
	}
}

func cloudQueuedRetryFloorByJob(database *sql.DB, candidates map[int64]*db.Job) map[int64]int64 {
	floors := make(map[int64]int64)
	if database == nil || len(candidates) == 0 {
		return floors
	}
	jobIDs := make([]int64, 0, len(candidates))
	for jobID := range candidates {
		jobIDs = append(jobIDs, jobID)
	}
	placeholders := make([]string, 0, len(jobIDs))
	args := make([]any, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		placeholders = append(placeholders, "?")
		args = append(args, jobID)
	}
	query := fmt.Sprintf(`SELECT cur.job_id,
		       COALESCE(prev.end_time, cur.queued_at, 0)
		  FROM job_attempts cur
		  JOIN job_attempts prev ON prev.id = cur.predecessor_attempt_id
		 WHERE cur.job_id IN (%s)
		   AND cur.id = (SELECT MAX(id) FROM job_attempts WHERE job_id = cur.job_id)
		   AND cur.status = ?
		   AND cur.launch_id IS NOT NULL
		   AND EXISTS (
		         SELECT 1
		           FROM job_attempts source
		          WHERE source.job_id = cur.job_id
		            AND source.launch_id = cur.launch_id
		            AND source.id < prev.id
		       )
		   AND prev.cloud_outcome = ?`, strings.Join(placeholders, ","))
	args = append(args, db.StatusQueued, db.AttemptOutcomeOrphaned)
	rows, err := database.Query(query, args...)
	if err != nil {
		return floors
	}
	defer rows.Close()
	for rows.Next() {
		var jobID int64
		var floor int64
		if err := rows.Scan(&jobID, &floor); err != nil {
			continue
		}
		if floor <= 0 {
			if job := candidates[jobID]; job != nil {
				floor = job.QueuedAt
				if floor <= 0 {
					floor = job.CreatedAt
				}
			}
		}
		floors[jobID] = floor
	}
	return floors
}

func relaunchRunawayBlockedReasonsWithFloor(database *sql.DB, floorByJob map[int64]int64) map[int64]string {
	reasons := make(map[int64]string)
	if database == nil || len(floorByJob) == 0 {
		return reasons
	}
	minFloor := int64(0)
	for _, floor := range floorByJob {
		if floor <= 0 {
			continue
		}
		if minFloor == 0 || floor < minFloor {
			minFloor = floor
		}
	}
	rows, err := database.Query(`SELECT occurred_at, event_kind, COALESCE(campaign_id, 0), COALESCE(detail, '')
		FROM lifecycle_events
		WHERE event_kind IN (?, ?)
		  AND (? = 0 OR occurred_at >= ?)
		ORDER BY occurred_at DESC, id DESC`, db.EventRelaunchRunawayBlocked, db.EventRelaunchRunawayResumed, minFloor, minFloor)
	if err != nil {
		return reasons
	}
	defer rows.Close()
	type runawayScopeKey struct {
		campaignID int64
		project    string
	}
	latestResumeByScope := map[runawayScopeKey]int64{}
	for rows.Next() {
		var (
			occurredAt int64
			eventKind  string
			campaignID int64
			detail     string
		)
		if err := rows.Scan(&occurredAt, &eventKind, &campaignID, &detail); err != nil {
			continue
		}
		scope := runawayScopeKey{
			campaignID: campaignID,
			project:    runawayProjectLabelFromDetail(detail),
		}
		if eventKind == db.EventRelaunchRunawayResumed {
			if occurredAt > latestResumeByScope[scope] {
				latestResumeByScope[scope] = occurredAt
			}
			continue
		}
		globalResumeAt := latestResumeByScope[runawayScopeKey{project: "<all>"}]
		scopeResumeAt := latestResumeByScope[scope]
		if globalResumeAt > scopeResumeAt {
			scopeResumeAt = globalResumeAt
		}
		if occurredAt <= scopeResumeAt {
			continue
		}
		reason := summarizeRunawayBlockedReason(detail)
		for jobID, floor := range floorByJob {
			if _, exists := reasons[jobID]; exists {
				continue
			}
			if floor > 0 && occurredAt < floor {
				continue
			}
			reasons[jobID] = reason
		}
		if len(reasons) == len(floorByJob) {
			break
		}
	}
	return reasons
}

func runawayProjectLabelFromDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	const key = "project="
	start := strings.Index(detail, key)
	if start < 0 {
		return "<all>"
	}
	value := detail[start+len(key):]
	if end := strings.Index(value, ";"); end >= 0 {
		value = value[:end]
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "<all>"
	}
	return value
}

func summarizeRunawayBlockedReason(detail string) string {
	detail = strings.TrimSpace(detail)
	if idx := strings.Index(detail, ";"); idx >= 0 {
		detail = strings.TrimSpace(detail[idx+1:])
	}
	if detail == "" {
		detail = "paused: repeated launch failures without progress"
	}
	return "new-instance retry blocked: " + detail
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
