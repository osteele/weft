package orchestration

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
)

// SourceSnapshot captures the pre-move state of a job's source attempt so the
// on-prem source process can be killed after the destination commits.
// Cloud-source jobs do not need a snapshot: the target-acceptance path writes
// an R2 cancel marker to the source instance for them.
type SourceSnapshot struct {
	JobID     int64
	Host      string // on-prem host of the source attempt
	StartTime int64  // start_time at snapshot — pid file is keyed on this
}

// SnapshotForcedSources returns SourceSnapshots for the running/starting/
// paused jobs whose source processes will need an SSH kill after a successful
// --force move. Jobs in any other state, or jobs not running on an inventory
// host, are skipped.
func SnapshotForcedSources(jobs []*db.Job) []SourceSnapshot {
	out := make([]SourceSnapshot, 0)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		status := job.EffectiveStatus()
		if status != db.StatusRunning && status != db.StatusStarting && status != db.StatusPaused {
			continue
		}
		if !job.HasInventoryHost() {
			continue
		}
		out = append(out, SourceSnapshot{
			JobID:     job.ID,
			Host:      job.Host,
			StartTime: job.StartTime,
		})
	}
	return out
}

// TerminateForcedSources SSH-kills each on-prem source process listed. Caller
// invokes this only after the destination has accepted the move and the
// central DB has abandoned the source attempt. Failures are reported via warn
// rather than returned; a failed kill leaves the source process running but
// orphaned (consuming host CPU but producing no DB-visible output).
func TerminateForcedSources(_ *sql.DB, snapshots []SourceSnapshot, warn func(string)) {
	for _, snap := range snapshots {
		job := &db.Job{ID: snap.JobID, Host: snap.Host, StartTime: snap.StartTime}
		if err := ops.KillQueueRunnerJob(job, 30*time.Second); err != nil && warn != nil {
			warn(fmt.Sprintf("--force: terminate source for %s on %s: %v", ids.FormatJobID(snap.JobID), snap.Host, err))
		}
	}
}
