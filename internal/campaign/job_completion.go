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
		return false
	}

	runID := int64(0)
	if latestRunID.Valid {
		runID = latestRunID.Int64
	}

	// Check for .complete marker
	completeKey := r2keys.JobAttemptComplete(jobID, runID)
	exists, err := r2c.ObjectExists(ctx, completeKey)
	if err != nil || !exists {
		return false
	}

	// Download results to parse exit code and times
	resultPrefix := r2keys.JobAttemptResultsPrefix(jobID, runID)
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-complete-%d-*", jobID))
	if err != nil {
		return false
	}
	defer os.RemoveAll(tmpDir)

	if err := r2c.DownloadResults(ctx, resultPrefix, tmpDir); err != nil {
		slog.Warn("failed to download results for job", "component", "watch", "job_id", jobID, "error", err)
		return false
	}

	jobIDStr := strconv.FormatInt(jobID, 10)
	exitCode, startTimeUnix, endTimeUnix, failureReason := db.ParseCloudJobResult(tmpDir, jobIDStr)
	if exitCode == nil {
		return false
	}

	// If no start_time from completion record, try .started marker
	if startTimeUnix == 0 {
		if data, err := r2c.GetObject(ctx, r2keys.JobAttemptStarted(jobID, runID)); err == nil {
			startTimeUnix, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		}
	}

	if _, err := db.RecordCloudJobCompletion(database, jobID, *exitCode, startTimeUnix, endTimeUnix, failureReason); err != nil {
		slog.Warn("failed to record completion for job", "component", "watch", "job_id", jobID, "error", err)
		return false
	}

	// Don't clean up R2 markers here — leave them for the full sync pass
	// which also imports phase timings and caches logs.

	slog.Info("synced completion for job", "component", "watch", "job_id", jobID, "exit_code", *exitCode)
	return true
}
