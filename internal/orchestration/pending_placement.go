package orchestration

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/osteele/weft/internal/db"
)

func markJobsPendingPlacement(database *sql.DB, jobIDs []int64) ([]int64, error) {
	if database == nil || len(jobIDs) == 0 {
		return nil, nil
	}

	ids := uniqueSortedJobIDs(jobIDs)
	marked := make([]int64, 0, len(ids))
	for _, jobID := range ids {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return marked, fmt.Errorf("get job %d: %w", jobID, err)
		}
		if job == nil {
			continue
		}
		status := job.EffectiveStatus()
		if status != db.StatusQueued && status != db.StatusPendingPlacement {
			continue
		}
		if err := db.SetPendingStatus(database, jobID, db.StatusPendingPlacement); err != nil {
			return marked, fmt.Errorf("set job %d pending_placement: %w", jobID, err)
		}
		marked = append(marked, jobID)
	}
	return marked, nil
}

func restorePendingPlacementToQueued(database *sql.DB, jobIDs []int64) error {
	if database == nil || len(jobIDs) == 0 {
		return nil
	}

	for _, jobID := range uniqueSortedJobIDs(jobIDs) {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return fmt.Errorf("get job %d: %w", jobID, err)
		}
		if job == nil || job.EffectiveStatus() != db.StatusPendingPlacement {
			continue
		}
		if err := db.SetPendingStatus(database, jobID, db.StatusQueued); err != nil {
			return fmt.Errorf("set job %d queued: %w", jobID, err)
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
