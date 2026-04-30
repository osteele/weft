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

// openPlacementWindow flips the matched jobs to pending_placement and
// records a PlacementIntent for each. operation is a short tag that ends
// up in oplog and the placement_intents.operation column.
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
		if job == nil {
			continue
		}
		status := job.EffectiveStatus()
		if status != db.StatusQueued && status != db.StatusPendingPlacement {
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
		if err := db.SetPendingStatus(database, jobID, db.StatusPendingPlacement); err != nil {
			_ = db.ResolvePlacementIntent(database, intent.ID, db.PlacementIntentStateCanceled, "set pending_placement failed")
			return w, fmt.Errorf("set job %s pending_placement: %w", ids.FormatJobID(jobID), err)
		}
		w.JobIDs = append(w.JobIDs, jobID)
		w.IntentIDs = append(w.IntentIDs, intent.ID)
	}
	return w, nil
}

// closePlacementWindow ends the window by restoring pending_placement to
// queued (where still set) and resolving the matching intents. success
// determines the resolution state. Safe to call from a defer; subsequent
// calls are no-ops (intent state guards both updates).
func closePlacementWindow(database *sql.DB, w PlacementWindow, success bool) error {
	if database == nil {
		return nil
	}
	for i, jobID := range w.JobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
		}
		if job != nil && job.EffectiveStatus() == db.StatusPendingPlacement {
			if err := db.SetPendingStatus(database, jobID, db.StatusQueued); err != nil {
				return fmt.Errorf("set job %s queued: %w", ids.FormatJobID(jobID), err)
			}
		}
		state := db.PlacementIntentStateConfirmed
		resolution := "placement succeeded"
		if !success {
			state = db.PlacementIntentStateCanceled
			resolution = "placement failed"
		}
		if err := db.ResolvePlacementIntent(database, w.IntentIDs[i], state, resolution); err != nil {
			slog.Warn("resolve placement intent",
				"component", "placement", "intent_id", w.IntentIDs[i], "job_id", jobID, "error", err)
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
