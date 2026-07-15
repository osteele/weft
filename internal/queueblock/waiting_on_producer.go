package queueblock

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/runner"
)

// ProducerWait describes one unsatisfied artifact dependency edge.
type ProducerWait struct {
	Path           string
	ProducerID     int64
	ProducerStatus string
}

func (w ProducerWait) Reason() string {
	return formatProducerWait(w.Path, w.ProducerID, w.ProducerStatus)
}

// WaitingOnProducerReason inspects job.Needs and returns a human-readable
// reason if any declared producer is not yet ready. Returns ("", false) when
// the job has no needs or all producers have terminally completed with an R2
// upload path. DB-only (no R2 HEADs), so safe to call from display paths.
func WaitingOnProducerReason(database *sql.DB, job *db.Job) (string, bool) {
	chain := TraceWaitingOnProducer(database, job)
	if len(chain) == 0 {
		return "", false
	}
	return chain[0].Reason(), true
}

// TraceWaitingOnProducer returns the first unsatisfied producer dependency and
// then follows that producer's own unsatisfied dependency, if any. This lets
// diagnose surfaces explain both "this job waits on wjB" and "wjB is queued
// because it waits on root producer wjA".
func TraceWaitingOnProducer(database *sql.DB, job *db.Job) []ProducerWait {
	if database == nil || job == nil {
		return nil
	}
	return traceWaitingOnProducer(database, job, make(map[int64]struct{}))
}

func traceWaitingOnProducer(database *sql.DB, job *db.Job, seen map[int64]struct{}) []ProducerWait {
	if job == nil || len(job.Needs) == 0 {
		return nil
	}
	if _, ok := seen[job.ID]; ok {
		return nil
	}
	seen[job.ID] = struct{}{}
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
			return []ProducerWait{{
				Path:           parsed.Path,
				ProducerID:     parsed.Version,
				ProducerStatus: "producer not found",
			}}
		}
		status := producer.EffectiveStatus()
		if !db.IsTerminalStatus(status) {
			head := ProducerWait{Path: parsed.Path, ProducerID: producer.ID, ProducerStatus: status}
			return append([]ProducerWait{head}, traceWaitingOnProducer(database, producer, seen)...)
		}
		if status != db.StatusCompleted {
			// Failed / canceled / etc. — the artifact will never appear in R2;
			// surface the producer state so the user can intervene.
			return []ProducerWait{{Path: parsed.Path, ProducerID: producer.ID, ProducerStatus: status}}
		}
		if producer.HasInventoryHost() {
			// Completed on-prem only; no R2 copy was uploaded.
			return []ProducerWait{{
				Path:           parsed.Path,
				ProducerID:     producer.ID,
				ProducerStatus: "on-prem, not in R2",
			}}
		}
	}
	return nil
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
