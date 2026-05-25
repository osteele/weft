package queueblock

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/runner"
)

// WaitingOnProducerReason inspects job.Needs and returns a human-readable
// reason if any declared producer is not yet ready. Returns ("", false) when
// the job has no needs or all producers have terminally completed with an R2
// upload path. DB-only (no R2 HEADs), so safe to call from display paths.
func WaitingOnProducerReason(database *sql.DB, job *db.Job) (string, bool) {
	if database == nil || job == nil || len(job.Needs) == 0 {
		return "", false
	}
	for _, spec := range job.Needs {
		parsed, err := runner.ParseNeedsSpec(spec)
		if err != nil {
			// Malformed spec is a separate error class; let the launch path
			// surface it.
			continue
		}
		// Named assets are not produced by another job; nothing to wait on.
		if parsed.IsAsset() {
			continue
		}
		producer, err := db.GetJobByID(database, parsed.Version)
		if err != nil || producer == nil {
			return formatProducerWait(parsed.Path, parsed.Version, "producer not found"), true
		}
		status := producer.EffectiveStatus()
		if !db.IsTerminalStatus(status) {
			return formatProducerWait(parsed.Path, producer.ID, status), true
		}
		if status != db.StatusCompleted {
			// Failed / canceled / etc. — the artifact will never appear in R2;
			// surface the producer state so the user can intervene.
			return formatProducerWait(parsed.Path, producer.ID, status), true
		}
		if producer.HasInventoryHost() {
			// Completed on-prem only; no R2 copy was uploaded.
			return formatProducerWait(parsed.Path, producer.ID, "on-prem, not in R2"), true
		}
	}
	return "", false
}

func formatProducerWait(path string, producerID int64, suffix string) string {
	return fmt.Sprintf("waiting for %q from %s (%s)", path, ids.FormatJobID(producerID), suffix)
}

// HydrateWaitingOnProducerReasons fills QueueBlockedReason on unplaced jobs
// that are still missing a reason, by inspecting their --needs producers'
// current state. Acts as the offline fallback when no recent autopilot pass
// has recorded a relaunch.skipped.waiting_on_producer event.
func HydrateWaitingOnProducerReasons(database *sql.DB, jobs []*db.Job) {
	if database == nil || len(jobs) == 0 {
		return
	}
	for _, job := range jobs {
		if job == nil || strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		if !job.IsUnplacedAwaitingPlacement() {
			continue
		}
		if reason, blocked := WaitingOnProducerReason(database, job); blocked {
			job.QueueBlockedReason = reason
		}
	}
}
