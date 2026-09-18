package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// syncJobStatusFromR2 attempts to sync a job's completion status from R2.
// This is used as a fallback when SSH to an inventory host fails.
// Returns (result, nil) on success, or an error if R2 data is unavailable.
func syncJobStatusFromR2(database *sql.DB, job *db.Job) (SyncResult, error) {
	r2Client, err := newInventoryR2Client()
	if err != nil {
		return SyncResult{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return syncJobStatusFromR2WithClient(ctx, r2Client, database, job)
}

func syncJobStatusFromR2WithClient(ctx context.Context, client r2ObjectGetter, database *sql.DB, job *db.Job) (SyncResult, error) {
	if job.LatestRunID == nil || *job.LatestRunID <= 0 {
		return SyncResult{}, fmt.Errorf("job %s has no current attempt for R2 completion sync", ids.FormatJobID(job.ID))
	}
	runID := *job.LatestRunID

	var (
		exitCode     int
		haveExitCode bool
		startTime    int64
		endTime      int64
	)

	completeKey := r2keys.JobAttemptComplete(job.ID, runID)
	if data, err := client.GetObject(ctx, completeKey); err == nil {
		if code, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
			exitCode = code
			haveExitCode = true
		}
	}

	// Read completion record for this exact attempt. This serves both to backfill
	// timing and as an authoritative fallback for the exit code if the .complete
	// marker was not written or was lost.
	completionKey := r2keys.JobAttemptResultsPrefix(job.ID, runID) + fmt.Sprintf("%d.completion.json", job.ID)
	if compData, compErr := client.GetObject(ctx, completionKey); compErr == nil {
		completionRunID, parsedExit, parsedStart, parsedEnd := parseCompletionRecord(compData)
		if completionRunID != runID {
			return SyncResult{}, fmt.Errorf(
				"R2 completion record run mismatch for job %s: got %d, want %d",
				ids.FormatJobID(job.ID), completionRunID, runID,
			)
		}
		startTime, endTime = parsedStart, parsedEnd
		if !haveExitCode && parsedExit != nil {
			exitCode = *parsedExit
			haveExitCode = true
		}
	}

	if !haveExitCode {
		return SyncResult{}, fmt.Errorf("R2 completion not found for job %s run %d", ids.FormatJobID(job.ID), runID)
	}

	if endTime == 0 {
		endTime = time.Now().Unix()
	}

	if err := RecordJobAttemptCompletion(database, job.ID, runID, exitCode, startTime, endTime); err != nil {
		return SyncResult{}, fmt.Errorf("record R2 completion for job %s run %d: %w", ids.FormatJobID(job.ID), runID, err)
	}

	return SyncResult{Updated: true, HostContacted: false}, nil
}

type r2ObjectGetter interface {
	GetObject(context.Context, string) ([]byte, error)
}

func newInventoryR2Client() (*r2.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	r2Cfg := cfg.Vastai.R2
	if r2Cfg.Bucket == "" || r2Cfg.AccessKeyID == "" {
		return nil, fmt.Errorf("R2 not configured")
	}
	client, err := r2.New(r2.Config{
		AccountID:       r2Cfg.AccountID,
		AccessKeyID:     r2Cfg.AccessKeyID,
		SecretAccessKey: r2Cfg.SecretAccessKey,
		Bucket:          r2Cfg.Bucket,
	})
	if err != nil {
		return nil, fmt.Errorf("create R2 client: %w", err)
	}
	return client, nil
}

// syncInventoryPublicationReports imports attempt-scoped readiness reports for
// R2-pull inventory hosts. These jobs have no rental launch, so the cloud
// result sweep does not select them even though they publish the same report.
func syncInventoryPublicationReports(database *sql.DB, jobs []*db.Job) int {
	client, err := newInventoryR2Client()
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return syncInventoryPublicationReportsWithClient(ctx, client, database, jobs)
}

func syncInventoryPublicationReportsWithClient(ctx context.Context, client r2ObjectGetter, database *sql.DB, jobs []*db.Job) int {
	updated := 0
	for _, job := range jobs {
		if job == nil || job.LatestRunID == nil || *job.LatestRunID <= 0 {
			continue
		}
		runID := *job.LatestRunID
		data, err := client.GetObject(ctx, r2keys.JobAttemptPublicationReport(job.ID, runID))
		if err != nil || len(data) == 0 {
			continue
		}
		changed, err := db.IngestAttemptPublicationReport(database, data, job.ID, runID)
		if err != nil {
			slog.Debug("failed to ingest inventory publication report", "component", "sync", "job_id", job.ID, "run_id", runID, "error", err)
			continue
		}
		if changed {
			updated++
		}
	}
	return updated
}

// parseCompletionRecord extracts run_id, exit_code, start_time, and end_time from a
// runner completion record (<id>.completion.json). Missing or malformed
// fields yield zeros or nil exit_code.
func parseCompletionRecord(data []byte) (runID int64, exitCode *int, startTime, endTime int64) {
	var rec struct {
		RunID     int64 `json:"run_id"`
		ExitCode  *int  `json:"exit_code"`
		StartTime int64 `json:"start_time"`
		EndTime   int64 `json:"end_time"`
	}
	if json.Unmarshal(data, &rec) != nil {
		return 0, nil, 0, 0
	}
	return rec.RunID, rec.ExitCode, rec.StartTime, rec.EndTime
}

// parseCompletionRecordTimes extracts run_id, start_time, and end_time from a
// runner completion record (<id>.completion.json). Missing or malformed
// fields yield zeros.
func parseCompletionRecordTimes(data []byte) (runID, startTime, endTime int64) {
	runID, _, startTime, endTime = parseCompletionRecord(data)
	return runID, startTime, endTime
}
