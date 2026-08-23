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

	// Check for .complete marker
	completeKey := r2keys.JobComplete(job.ID)
	data, err := r2Client.GetObject(ctx, completeKey)
	if err != nil {
		return SyncResult{}, fmt.Errorf("R2 .complete not found for job %s", ids.FormatJobID(job.ID))
	}

	exitCode, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return SyncResult{}, fmt.Errorf("parse R2 exit code for job %s: %w", ids.FormatJobID(job.ID), err)
	}

	// Try to read the completion record for start/end times. Backfilling
	// start_time here matters: this path can be the only one that ever
	// records this completion (SSH stayed unavailable), and once the attempt
	// closes with end_time set, a missing start_time is otherwise never
	// recovered.
	var startTime, endTime int64
	completionKey := r2keys.JobResultsPrefix(job.ID) + fmt.Sprintf("%d.completion.json", job.ID)
	if compData, compErr := r2Client.GetObject(ctx, completionKey); compErr == nil {
		startTime, endTime = parseCompletionRecordTimes(compData)
	}
	if endTime == 0 {
		endTime = time.Now().Unix()
	}

	if job.StartTime == 0 && startTime > 0 {
		if dbErr := db.UpdateStartTime(database, job.ID, startTime); dbErr == nil {
			job.StartTime = startTime
		}
	}

	if err := RecordJobCompletion(database, job.ID, exitCode, endTime); err != nil {
		return SyncResult{}, fmt.Errorf("record R2 completion for job %s: %w", ids.FormatJobID(job.ID), err)
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

// parseCompletionRecordTimes extracts start_time and end_time from a
// runner completion record (<id>.completion.json). Missing or malformed
// fields yield zeros.
func parseCompletionRecordTimes(data []byte) (startTime, endTime int64) {
	var rec struct {
		StartTime int64 `json:"start_time"`
		EndTime   int64 `json:"end_time"`
	}
	if json.Unmarshal(data, &rec) != nil {
		return 0, 0
	}
	return rec.StartTime, rec.EndTime
}
