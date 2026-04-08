package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// CheckAndSyncJobComplete checks whether R2 has a .complete marker for a
// specific job and, if so, records the completion in the DB. This provides a
// fast path for job completion during watch: instead of waiting for the full
// syncCloudJobResults scan, the watcher calls this when it detects a phase
// transition away from a job.
//
// Phase timings and log caching are deferred to the full sync pass to avoid
// an import cycle with internal/coordinator.
//
// Returns true if completion was recorded.
func CheckAndSyncJobComplete(ctx context.Context, r2c *r2.Client, database *sql.DB, jobID int64) bool {
	if !r2c.IsConfigured() {
		return false
	}

	// Look up current status and run ID
	var currentStatus string
	var latestRunID sql.NullInt64
	if err := database.QueryRow(
		"SELECT status, latest_run_id FROM job_status WHERE id = ? AND tombstoned = 0",
		jobID,
	).Scan(&currentStatus, &latestRunID); err != nil {
		return false
	}
	if db.IsTerminalStatus(currentStatus) {
		needsBackfill, err := db.NeedsCloudCompletionBackfill(database, jobID)
		if err != nil {
			slog.Warn("failed to evaluate cloud completion backfill need",
				"component", "reconcile", "job_id", jobID, "error", err)
			return false
		}
		if !needsBackfill {
			slog.Debug("skipping completion sync for terminal job with complete metadata",
				"component", "reconcile", "job_id", jobID, "reason", "terminal_complete_skip")
			return false
		}
		slog.Debug("attempting completion backfill for terminal job",
			"component", "reconcile", "job_id", jobID, "reason", "terminal_incomplete_backfill")
	}

	runID := int64(0)
	if latestRunID.Valid {
		runID = latestRunID.Int64
	}

	// Check for .complete marker
	completeKey := r2keys.JobAttemptComplete(jobID, runID)
	markerData, markerErr := r2c.GetObject(ctx, completeKey)
	if markerErr != nil || len(markerData) == 0 {
		return false
	}

	// Download results to parse exit code and times
	resultPrefix := r2keys.JobAttemptResultsPrefix(jobID, runID)
	var exitCode *int
	var startTimeUnix, endTimeUnix int64
	var failureReason string
	source := "results"

	if tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-complete-%d-*", jobID)); err == nil {
		defer os.RemoveAll(tmpDir)
		if err := r2c.DownloadResults(ctx, resultPrefix, tmpDir); err != nil {
			slog.Warn("results download failed, trying .complete marker fallback",
				"component", "reconcile", "job_id", jobID, "run_id", runID, "error", err)
		} else {
			jobIDStr := strconv.FormatInt(jobID, 10)
			exitCode, startTimeUnix, endTimeUnix, failureReason = db.ParseCloudJobResult(tmpDir, jobIDStr)
		}
	}

	// Fallback to .complete marker content if results haven't been uploaded yet
	if exitCode == nil {
		if code, parseErr := strconv.Atoi(strings.TrimSpace(string(markerData))); parseErr == nil {
			exitCode = &code
			source = "marker-fallback"
			slog.Debug("using exit code from .complete marker (results not yet available)",
				"component", "reconcile", "job_id", jobID, "run_id", runID, "exit_code", code)
		} else {
			slog.Warn("failed to parse exit code from .complete marker",
				"component", "reconcile", "job_id", jobID, "run_id", runID,
				"marker_content", string(markerData), "error", parseErr)
			return false
		}
	}

	// If no start_time from completion record, try .started marker
	if startTimeUnix == 0 {
		if data, err := r2c.GetObject(ctx, r2keys.JobAttemptStarted(jobID, runID)); err == nil {
			startTimeUnix, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		}
	}

	launchID, err := db.RecordCloudJobCompletion(database, jobID, *exitCode, startTimeUnix, endTimeUnix, failureReason)
	if err != nil {
		slog.Warn("failed to record completion for job",
			"component", "reconcile", "job_id", jobID, "source", source, "error", err)
		return false
	}

	// Log cloud job completion/failure to local ops log
	host := ""
	if launchID > 0 {
		host = db.LaunchHost(launchID)
	}
	if *exitCode == 0 {
		oplog.LogJob(oplog.OpJobComplete, jobID, host, oplog.WithDetailf("cloud exit=0 source=%s", source))
	} else {
		oplog.LogJob(oplog.OpJobFail, jobID, host, oplog.WithDetailf("cloud exit=%d source=%s", *exitCode, source))
	}

	// Don't clean up R2 markers here — leave them for the full sync pass
	// which also imports phase timings and caches logs.

	slog.Debug("synced job completion", "component", "reconcile", "job_id", jobID,
		"exit_code", *exitCode, "source", source)
	return true
}

// FinalizeStuckJobsWithR2Check finds non-terminal jobs on completed launches
// and attempts R2 sync before marking them dead. This prevents incorrectly
// marking jobs as dead when the agent uploaded results after the reconciler's
// initial R2 sync but before this safety-net runs.
func FinalizeStuckJobsWithR2Check(database *sql.DB, r2Client *r2.Client) ([]int64, error) {
	stuck, err := db.FindStuckJobsOnCompletedLaunches(database)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	var finalized []int64
	for _, s := range stuck {
		// Try R2 sync first — the .complete marker may have arrived since
		// the reconciler last checked.
		if r2Client != nil && reconcileCheckAndSyncJobComplete(ctx, r2Client, database, s.JobID) {
			slog.Debug("recovered stuck job from R2 (would have been marked dead)",
				"component", "sync", "job_id", s.JobID)
			finalized = append(finalized, s.JobID)
			continue
		}

		// R2 sync failed — mark as dead
		if err := db.MarkStuckJobDead(database, s.AttemptID); err != nil {
			return finalized, err
		}
		finalized = append(finalized, s.JobID)
	}
	return finalized, nil
}
