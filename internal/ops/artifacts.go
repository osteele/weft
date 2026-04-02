package ops

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
)

// RecordJobOutputs registers a completed job's declared outputs as data assets
// on the host where the job ran. This makes outputs immediately visible for
// transfer cost estimation and pre-staging of downstream jobs.
//
// Only records outputs for successfully completed jobs (exit code 0).
// Silently does nothing if the job has no outputs or didn't succeed.
func RecordJobOutputs(database *sql.DB, job *db.Job) {
	if job == nil || len(job.Outputs) == 0 {
		return
	}
	// Only record on success
	if job.Status != db.StatusCompleted || job.ExitCode == nil || *job.ExitCode != 0 {
		return
	}

	now := time.Now()
	recorded := 0
	for _, ref := range job.Outputs {
		asset, outputPath, ok := parseRecordedOutputRef(job.ID, ref)
		if !ok {
			continue
		}
		entry := dataloc.HostDataEntry{
			Host:     job.Host,
			Asset:    asset,
			Path:     outputPath,
			LastSeen: now,
		}
		if err := dataloc.RecordAsset(database, entry); err != nil {
			continue
		}
		recorded++
	}

	if recorded > 0 {
		oplog.LogJob("job.record-outputs", job.ID, job.Host,
			oplog.WithDetailf("%d/%d outputs recorded", recorded, len(job.Outputs)))
	}
}

func parseRecordedOutputRef(jobID int64, ref string) (dataloc.DataAsset, string, bool) {
	if asset, ok := dataloc.ParseAssetRef(ref); ok {
		return asset, "", true
	}
	if !strings.HasPrefix(ref, "local:") {
		return dataloc.DataAsset{}, "", false
	}

	relPath := filepath.Clean(strings.TrimSpace(strings.TrimPrefix(ref, "local:")))
	if relPath == "" || relPath == "." {
		return dataloc.DataAsset{}, "", false
	}

	return dataloc.DataAsset{
		Kind: dataloc.AssetJobOutput,
		ID:   fmt.Sprintf("%d/%s", jobID, relPath),
	}, relPath, true
}
