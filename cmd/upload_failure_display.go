package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2upload"
)

// printUploadFailureForJob is a best-effort lookup of the upload-failure
// marker for the job's latest cloud attempt. When present, prints a
// single-line "Upload:" diagnosis. Silently skipped if R2 is unreachable
// or the job has no cloud attempt — this is informational, not load-bearing.
func printUploadFailureForJob(database *sql.DB, job *db.Job) {
	if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
		return // not a cloud job
	}
	attempts, err := db.ListAttempts(database, job.ID)
	if err != nil || len(attempts) == 0 {
		return
	}
	latest := attempts[0]
	if latest.LaunchID == nil || *latest.LaunchID <= 0 {
		return
	}
	r2Client, err := newR2Client()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	marker, ok := fetchJobUploadFailureMarker(ctx, r2Client, job.ID, latest.ID)
	if !ok {
		return
	}
	fmt.Printf("Upload:       truncated — %s (%s); %d/%d bytes in %.0fs\n",
		marker.Cause(), marker.Reason,
		marker.BytesUploaded, marker.BytesTotal, marker.ElapsedSeconds)
}

// fetchJobUploadFailureMarker tries jobs/<id>/runs/<run>/upload-failure.json
// then the run=0 fallback. Returns (marker, true) on first hit.
func fetchJobUploadFailureMarker(ctx context.Context, r2Client *r2.Client, jobID, runID int64) (r2upload.FailureMarker, bool) {
	candidates := []int64{runID}
	if runID != 0 {
		candidates = append(candidates, 0)
	}
	for _, id := range candidates {
		key := r2keys.JobAttemptUploadFailure(jobID, id)
		exists, err := r2Client.ObjectExists(ctx, key)
		if err != nil || !exists {
			continue
		}
		data, err := r2Client.GetObject(ctx, key)
		if err != nil {
			continue
		}
		var m r2upload.FailureMarker
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		return m, true
	}
	return r2upload.FailureMarker{}, false
}
