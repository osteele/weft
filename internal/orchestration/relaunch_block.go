package orchestration

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
)

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

func RelaunchBlockedReasonsFromEvents(database *sql.DB, jobIDs []int64, sinceUnix int64) map[int64]string {
	reasons := make(map[int64]string)
	if database == nil || len(jobIDs) == 0 {
		return reasons
	}
	placeholders := make([]string, 0, len(jobIDs))
	for range jobIDs {
		placeholders = append(placeholders, "?")
	}
	query := fmt.Sprintf(`SELECT job_id, event_kind, detail, attempt_number, max_attempts
		FROM lifecycle_events
		WHERE event_kind LIKE 'relaunch.skipped.%%'
		  AND job_id IN (%s)`, strings.Join(placeholders, ","))
	if sinceUnix > 0 {
		query += "\n\t\t  AND occurred_at >= ?"
	}
	query += "\n\t\tORDER BY occurred_at DESC, id DESC"
	args := make([]any, 0, len(jobIDs)+1)
	for _, jobID := range jobIDs {
		args = append(args, jobID)
	}
	if sinceUnix > 0 {
		args = append(args, sinceUnix)
	}
	rows, err := database.Query(query, args...)
	if err != nil {
		return reasons
	}
	defer rows.Close()
	for rows.Next() {
		var (
			jobID         int64
			eventKind     string
			detail        sql.NullString
			attemptNumber int
			maxAttempts   int
		)
		if err := rows.Scan(&jobID, &eventKind, &detail, &attemptNumber, &maxAttempts); err != nil {
			continue
		}
		if _, exists := reasons[jobID]; exists {
			continue
		}
		reasons[jobID] = summarizeRelaunchSkipEvent(eventKind, detail.String, attemptNumber, maxAttempts)
	}
	return reasons
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
			attemptNumber int
			maxAttempts   int
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
		reasons[jobID] = summarizeRelaunchSkipEvent(eventKind, detail.String, attemptNumber, maxAttempts)
	}
	return reasons
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
