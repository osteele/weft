package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	cfg, err := config.Load()
	if err != nil {
		return SyncResult{}, fmt.Errorf("load config: %w", err)
	}

	r2Cfg := cfg.Vastai.R2
	if r2Cfg.Bucket == "" || r2Cfg.AccessKeyID == "" {
		return SyncResult{}, fmt.Errorf("R2 not configured")
	}

	r2Client, err := r2.New(r2.Config{
		AccountID:       r2Cfg.AccountID,
		AccessKeyID:     r2Cfg.AccessKeyID,
		SecretAccessKey: r2Cfg.SecretAccessKey,
		Bucket:          r2Cfg.Bucket,
	})
	if err != nil {
		return SyncResult{}, fmt.Errorf("create R2 client: %w", err)
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

	// Try to read the completion record for end_time
	var endTime int64
	completionKey := r2keys.JobResultsPrefix(job.ID) + fmt.Sprintf("%d.completion.json", job.ID)
	if compData, compErr := r2Client.GetObject(ctx, completionKey); compErr == nil {
		var rec struct {
			EndTime int64 `json:"end_time"`
		}
		if json.Unmarshal(compData, &rec) == nil && rec.EndTime > 0 {
			endTime = rec.EndTime
		}
	}
	if endTime == 0 {
		endTime = time.Now().Unix()
	}

	if err := RecordJobCompletion(database, job.ID, exitCode, endTime); err != nil {
		return SyncResult{}, fmt.Errorf("record R2 completion for job %s: %w", ids.FormatJobID(job.ID), err)
	}

	return SyncResult{Updated: true, HostContacted: false}, nil
}
