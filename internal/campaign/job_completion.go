package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/notify"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
	"github.com/osteele/weft/internal/status"
)

// Sync source labels reported in completion logs and used to gate post-sync
// cleanup (R2 .processed marker writes). SourceResults means the completion
// JSON was successfully ingested; SourceMarkerFallback means only the
// .complete marker's exit code was usable.
const (
	SourceResults        = "results"
	SourceMarkerFallback = "marker-fallback"
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
	var launchID sql.NullInt64
	if err := database.QueryRow(
		"SELECT status, latest_run_id, launch_id FROM job_status WHERE id = ? AND tombstoned = 0",
		jobID,
	).Scan(&currentStatus, &latestRunID, &launchID); err != nil {
		return false
	}
	if latestRunID.Valid {
		SyncAttemptPublicationReport(ctx, r2c, database, jobID, latestRunID.Int64)
	}
	allowAnyRunFallback := false
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
		allowAnyRunFallback = true
		slog.Debug("attempting completion backfill for terminal job",
			"component", "reconcile", "job_id", jobID, "reason", "terminal_incomplete_backfill")
	} else if nonTerminalCompletionFallbackAllowed(database, currentStatus, launchID, jobID) {
		allowAnyRunFallback = true
	}

	runID := int64(0)
	if latestRunID.Valid {
		runID = latestRunID.Int64
	}
	return checkAndSyncJobCompleteRun(ctx, r2c, database, jobID, runID, allowAnyRunFallback)
}

func CheckAndSyncJobCompleteRun(ctx context.Context, r2c *r2.Client, database *sql.DB, jobID, runID int64) bool {
	if !r2c.IsConfigured() || runID <= 0 {
		return false
	}
	return checkAndSyncJobCompleteRun(ctx, r2c, database, jobID, runID, false)
}

// ProcessedMarkerWriter is the subset of r2.Client needed to mark a completion
// as processed.
type ProcessedMarkerWriter interface {
	PutMarker(ctx context.Context, key string) error
}

// MarkRejectedCompletionProcessed writes the .processed marker for a
// completion that RecordCloudJobCompletion permanently rejected, so the
// .complete marker is not re-read and re-rejected on every sync pass.
// Transition-validation rejections and abandoned/missing attempt completions
// are permanent; transient errors (e.g. DB I/O) are left unprocessed so a
// later pass can retry. Returns true if the marker was written.
func MarkRejectedCompletionProcessed(ctx context.Context, w ProcessedMarkerWriter, jobID, runID int64, err error) bool {
	var ite *status.InvalidTransitionError
	if !errors.As(err, &ite) && !db.IsPermanentCloudCompletionNoop(err) {
		return false
	}
	_ = w.PutMarker(ctx, r2keys.JobAttemptProcessed(jobID, runID))
	return true
}

func nonTerminalCompletionFallbackAllowed(database *sql.DB, currentStatus string, launchID sql.NullInt64, jobID int64) bool {
	if !launchID.Valid {
		return false
	}
	switch currentStatus {
	case db.StatusStarting, db.StatusRunning, db.StatusPaused:
	default:
		return false
	}
	var phase string
	if err := database.QueryRow(
		`SELECT instance_phase FROM launch_live_state WHERE launch_id = ?`,
		launchID.Int64,
	).Scan(&phase); err != nil {
		return false
	}
	verb, phaseJobID, ok := ParsePhaseJobID(phase)
	return !(ok && verb == PhaseRunning && phaseJobID == jobID)
}

func checkAndSyncJobCompleteRun(ctx context.Context, r2c *r2.Client, database *sql.DB, jobID, runID int64, allowAnyRunFallback bool) bool {
	// Check for .complete marker at the latest_run_id; on miss, when the job
	// is terminal and needs backfill, scan the job's prefix for any other
	// run_id that has a marker (cleanupStaleAttempts may have advanced
	// latest_run_id past the run that actually wrote the marker).
	completeKey := r2keys.JobAttemptComplete(jobID, runID)
	SyncAttemptPublicationReport(ctx, r2c, database, jobID, runID)
	markerData, markerLastModified, markerErr := r2c.GetObjectWithMeta(ctx, completeKey)
	if markerErr != nil || len(markerData) == 0 {
		if !allowAnyRunFallback {
			return false
		}
		altRunID, ok := findAnyCompletedRunID(ctx, r2c, jobID)
		if !ok {
			return false
		}
		runID = altRunID
		SyncAttemptPublicationReport(ctx, r2c, database, jobID, runID)
		completeKey = r2keys.JobAttemptComplete(jobID, runID)
		markerData, markerLastModified, markerErr = r2c.GetObjectWithMeta(ctx, completeKey)
		if markerErr != nil || len(markerData) == 0 {
			return false
		}
		slog.Debug("recovered completion marker at older run_id",
			"component", "reconcile", "job_id", jobID, "run_id", runID,
			"reason", "run_id_mismatch_backfill")
	}

	// Skip completions already marked processed — either ingested by the full
	// sync pass, or permanently rejected by transition validation. Without
	// this gate a permanently-rejected completion would be re-downloaded and
	// re-rejected on every reconcile/backfill pass.
	if processed, err := r2c.ObjectExists(ctx, r2keys.JobAttemptProcessed(jobID, runID)); err == nil && processed {
		slog.Debug("skipping completion already marked processed",
			"component", "reconcile", "job_id", jobID, "run_id", runID)
		return false
	}

	// Download results to parse exit code and times
	resultPrefix := r2keys.JobAttemptResultsPrefix(jobID, runID)
	var exitCode *int
	var startTimeUnix, endTimeUnix int64
	var failureReason, killReason string
	source := SourceResults

	if tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-complete-%d-*", jobID)); err == nil {
		defer os.RemoveAll(tmpDir)
		if err := r2c.DownloadResults(ctx, resultPrefix, tmpDir); err != nil {
			slog.Warn("results download failed, trying .complete marker fallback",
				"component", "reconcile", "job_id", jobID, "run_id", runID, "error", err)
		} else {
			jobIDStr := strconv.FormatInt(jobID, 10)
			exitCode, startTimeUnix, endTimeUnix, failureReason, killReason = db.ParseCloudJobResult(tmpDir, jobIDStr)
			if _, ingestErr := db.IngestCloudJobPublicationReport(database, tmpDir, jobIDStr, jobID, runID); ingestErr != nil {
				slog.Warn("failed to ingest completion publication report",
					"component", "reconcile", "job_id", jobID, "run_id", runID, "error", ingestErr)
			}
		}
	}

	// Fallback to .complete marker content if results haven't been uploaded yet
	if exitCode == nil {
		if code, parseErr := strconv.Atoi(strings.TrimSpace(string(markerData))); parseErr == nil {
			exitCode = &code
			source = SourceMarkerFallback
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

	recordResult, err := db.RecordCloudJobCompletionWithTransition(database, jobID, *exitCode, startTimeUnix, endTimeUnix, failureReason, killReason, markerLastModified, runID)
	if err != nil {
		if MarkRejectedCompletionProcessed(ctx, r2c, jobID, runID, err) {
			slog.Debug("marked permanently unrecordable cloud completion processed",
				"component", "reconcile", "job_id", jobID, "run_id", runID, "source", source, "error", err)
			return false
		}
		slog.Warn("failed to record completion for job",
			"component", "reconcile", "job_id", jobID, "source", source, "error", err)
		return false
	}
	if source == SourceResults {
		persistCloudSourceVerification(database, jobID, runID, failureReason, markerLastModified)
	}
	launchID := recordResult.LaunchID

	// Log cloud job completion/failure to local ops log
	host := ""
	if launchID > 0 {
		host = db.LaunchHost(launchID)
	}
	if recordResult.Transitioned {
		if *exitCode == 0 {
			oplog.LogJob(oplog.OpJobComplete, jobID, host, oplog.WithDetailf("cloud exit=0 source=%s", source))
			notify.JobTerminal(database, jobID, db.StatusCompleted, exitCode)
		} else {
			oplog.LogJob(oplog.OpJobFail, jobID, host, oplog.WithDetailf("cloud exit=%d source=%s", *exitCode, source))
			notify.JobTerminal(database, jobID, db.StatusFailed, exitCode)
		}
	}

	// Don't clean up R2 markers here — leave them for the full sync pass
	// which also imports phase timings and caches logs.

	slog.Debug("synced job completion", "component", "reconcile", "job_id", jobID,
		"exit_code", *exitCode, "source", source)
	return true
}

// SyncAttemptPublicationReport ingests the durable standalone publication
// report. It is intentionally safe to call before or after completion-result
// cleanup because the report key is outside the results prefix.
func SyncAttemptPublicationReport(ctx context.Context, r2c *r2.Client, database *sql.DB, jobID, runID int64) bool {
	if r2c == nil || database == nil || runID <= 0 {
		return false
	}
	data, err := r2c.GetObject(ctx, r2keys.JobAttemptPublicationReport(jobID, runID))
	if err != nil || len(data) == 0 {
		return false
	}
	updated, err := db.IngestAttemptPublicationReport(database, data, jobID, runID)
	if err != nil {
		slog.Warn("failed to ingest publication report",
			"component", "reconcile", "job_id", jobID, "run_id", runID, "error", err)
		return false
	}
	return updated
}

// findAnyCompletedRunID scans R2 for any .complete marker under jobs/<jobID>/
// and returns the run_id of an arbitrary one. Used when the marker is missing
// at the DB's latest_run_id (e.g. after cleanupStaleAttempts advanced the
// run_id past the run that actually completed). Returns (0, false) if none.
func findAnyCompletedRunID(ctx context.Context, r2c *r2.Client, jobID int64) (int64, bool) {
	prefix := r2keys.JobPrefix(jobID) + "/"
	markers, err := r2c.ListJobMarkers(ctx, prefix)
	if err != nil || markers == nil {
		return 0, false
	}
	key, ok := markers.AnyCompletedKey(jobID)
	if !ok {
		return 0, false
	}
	return r2keys.ExtractRunID(key), true
}

// SourceManifest labels a completion ingested from the instance-level
// completion manifest rather than a per-job .complete marker.
const SourceManifest = "manifest"

// CreditManifestCompletions records successful per-job completions from the
// instance-level completion manifest (campaigns/<instanceID>/.complete) for any
// launch job still non-terminal in the DB.
//
// The agent writes the instance manifest only at self-destruct, strictly after
// each job's own .complete marker, so its presence is authoritative evidence
// that every job it lists with exit code 0 actually finished. Crediting those
// jobs here — before ExecuteAction runs CloseLaunchAttempts — closes a race:
// when an instance self-destructs quickly, the reconciler can see the
// instance-level marker before the per-job marker sync observes the per-job
// markers. Without this, CloseLaunchAttempts' completed-branch orphans and
// re-runs a job that already succeeded.
//
// Jobs the manifest reports with a non-zero exit code are left untouched for
// the normal failure/orphan path.
func CreditManifestCompletions(database *sql.DB, r2Client *r2.Client, instanceID int64) {
	if r2Client == nil {
		return
	}
	manifest := readR2CompletionManifest(r2Client, instanceID)
	if manifest == nil {
		return
	}
	creditManifestCompletions(database, instanceID, manifest)
}

// creditManifestCompletions is the DB-only core of CreditManifestCompletions,
// split out so it can be exercised without an R2 client.
func creditManifestCompletions(database *sql.DB, instanceID int64, manifest *runner.InstanceCompletionManifest) {
	if manifest == nil {
		return
	}
	jobs, err := db.GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		slog.Warn("credit manifest completions: load launch jobs",
			"component", "reconcile", "instance", instanceID, "error", err)
		return
	}
	outcomes, _ := db.GetAttemptOutcomesByLaunch(database, instanceID)
	jobByID := make(map[int64]*db.Job, len(jobs))
	for _, j := range jobs {
		jobByID[j.ID] = j
	}
	for _, summary := range manifest.Jobs {
		if summary.ExitCode != 0 {
			continue
		}
		j, ok := jobByID[summary.JobID]
		if !ok {
			continue
		}
		if IsJobTerminal(AttemptDisplayStatus(j, outcomes)) {
			continue
		}
		runID := int64(0)
		if j.LatestRunID != nil {
			runID = *j.LatestRunID
		}
		var marker time.Time
		if manifest.CompletedAtUnix > 0 {
			marker = time.Unix(manifest.CompletedAtUnix, 0)
		}
		// endTimeUnix == 0: leave last_synced_status NULL so the full sync
		// pass can still backfill authoritative phase timings.
		recordResult, err := db.RecordCloudJobCompletionWithTransition(database, summary.JobID, 0, 0, 0, "", "", marker, runID)
		if err != nil {
			if db.IsPermanentCloudCompletionNoop(err) {
				slog.Debug("skipping permanently unrecordable manifest completion",
					"component", "reconcile", "instance", instanceID,
					"job_id", summary.JobID, "run_id", runID, "error", err)
				continue
			}
			slog.Warn("credit manifest completion failed",
				"component", "reconcile", "instance", instanceID,
				"job_id", summary.JobID, "error", err)
			continue
		}
		persistCloudSourceVerification(database, summary.JobID, runID, "", marker)
		if recordResult.Transitioned {
			oplog.LogJob(oplog.OpJobComplete, summary.JobID, db.LaunchHost(instanceID),
				oplog.WithDetailf("cloud exit=0 source=%s", SourceManifest))
			manifestExit := 0
			notify.JobTerminal(database, summary.JobID, db.StatusCompleted, &manifestExit)
		}
		slog.Debug("credited job completion from instance manifest",
			"component", "reconcile", "instance", instanceID, "job_id", summary.JobID)
	}
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
