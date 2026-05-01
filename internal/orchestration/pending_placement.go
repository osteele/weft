package orchestration

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// PlacementWindow bundles the per-job state mutated when an orchestration
// path opens a placement window: the marked pending_status flip and the
// PlacementIntent that excludes the job from autopilot consideration.
// Callers open with openPlacementWindow and close with closePlacementWindow
// (typically via defer). closePlacementWindow is idempotent — calling it
// twice is harmless.
type PlacementWindow struct {
	JobIDs    []int64
	IntentIDs []int64
}

// openPlacementWindow records a PlacementIntent for each matched job.
// `operation` is a short tag that ends up in oplog and the
// placement_intents.operation column. The autopilot's exclusion is keyed
// on intent presence, so the window provides the "hands off this job"
// guarantee without needing a corresponding pending_status flip on the
// job_attempts row.
func openPlacementWindow(database *sql.DB, jobIDs []int64, operation string) (PlacementWindow, error) {
	w := PlacementWindow{}
	if database == nil || len(jobIDs) == 0 {
		return w, nil
	}
	for _, jobID := range uniqueSortedJobIDs(jobIDs) {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return w, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
		}
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		intent, err := db.CreatePlacementIntent(database, jobID, operation)
		if err != nil {
			// A job that already has an open intent is being placed by
			// another path; skip rather than disrupt it.
			if errors.Is(err, db.ErrPlacementIntentAlreadyOpen) {
				continue
			}
			return w, fmt.Errorf("open placement intent for job %s: %w", ids.FormatJobID(jobID), err)
		}
		w.JobIDs = append(w.JobIDs, jobID)
		w.IntentIDs = append(w.IntentIDs, intent.ID)
	}
	return w, nil
}

// closePlacementWindow resolves the matching intents. `success`
// determines the resolution state. Safe to call from a defer; subsequent
// calls are no-ops (the intent's state predicate guards the update).
func closePlacementWindow(database *sql.DB, w PlacementWindow, success bool) error {
	if database == nil {
		return nil
	}
	state := db.PlacementIntentStateConfirmed
	resolution := "placement succeeded"
	if !success {
		state = db.PlacementIntentStateCanceled
		resolution = "placement failed"
	}
	for _, intentID := range w.IntentIDs {
		if err := db.ResolvePlacementIntent(database, intentID, state, resolution); err != nil {
			slog.Warn("resolve placement intent",
				"component", "placement", "intent_id", intentID, "error", err)
		}
	}
	return nil
}

func uniqueSortedJobIDs(jobIDs []int64) []int64 {
	seen := make(map[int64]struct{}, len(jobIDs))
	ids := make([]int64, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		if jobID <= 0 {
			continue
		}
		if _, ok := seen[jobID]; ok {
			continue
		}
		seen[jobID] = struct{}{}
		ids = append(ids, jobID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
