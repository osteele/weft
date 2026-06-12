package ops

import (
	"database/sql"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ssh"
)

var snapshotRemoteJobOutputFunc = snapshotRemoteJobOutput

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
		recordPath := outputPath
		if outputPath != "" {
			snapshotPath, err := snapshotRemoteJobOutputFunc(database, job, outputPath, defaultSourceSyncTimeout)
			if err != nil {
				oplog.LogJob("job.record-outputs", job.ID, job.Host,
					oplog.WithDetailf("output snapshot failed for %s: %v", outputPath, err))
				continue
			}
			recordPath = snapshotPath
		}
		entry := dataloc.HostDataEntry{
			Host:     job.Host,
			Asset:    asset,
			Path:     recordPath,
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

func snapshotRemoteJobOutput(database *sql.DB, job *db.Job, relPath string, timeout time.Duration) (string, error) {
	if job == nil || strings.TrimSpace(job.Host) == "" || strings.TrimSpace(job.WorkingDir) == "" {
		return "", fmt.Errorf("job has no host or working directory")
	}
	relPath = filepath.ToSlash(filepath.Clean(strings.TrimSpace(relPath)))
	if relPath == "" || relPath == "." || strings.HasPrefix(relPath, "../") || relPath == ".." || strings.HasPrefix(relPath, "/") {
		return "", fmt.Errorf("invalid output path %q", relPath)
	}
	runID := int64(0)
	if job.LatestRunID != nil {
		runID = *job.LatestRunID
	} else if database != nil {
		id, err := db.GetLatestAttemptID(database, job.ID)
		if err != nil {
			return "", fmt.Errorf("get latest attempt: %w", err)
		}
		runID = id
	}
	snapshotPath := path.Join(artifacts.RemoteArtifactsDir, fmt.Sprintf("%d", job.ID), fmt.Sprintf("%d", runID), "outputs", relPath)
	sourcePath := remotePathJoin(job.WorkingDir, relPath)
	cmd := fmt.Sprintf(
		"src=%s; dst=%s; mkdir -p -- \"$(dirname -- \"$dst\")\" && if [ -d \"$src\" ]; then mkdir -p -- \"$dst\" && cp -a -- \"$src\"/. \"$dst\"/; else cp -p -- \"$src\" \"$dst\"; fi",
		shellQuoteRemotePath(sourcePath),
		shellQuoteRemotePath(snapshotPath),
	)
	_, stderr, err := ssh.RunWithTimeout(job.Host, cmd, timeout)
	if err != nil {
		if strings.TrimSpace(stderr) != "" {
			return "", fmt.Errorf("%s: %w", strings.TrimSpace(stderr), err)
		}
		return "", err
	}
	return snapshotPath, nil
}

func remotePathJoin(base, rel string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	rel = strings.TrimLeft(filepath.ToSlash(rel), "/")
	if base == "" {
		return rel
	}
	return base + "/" + rel
}

func shellQuoteRemotePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" {
		return "$HOME"
	}
	if strings.HasPrefix(p, "~/") {
		rest := strings.TrimPrefix(p, "~/")
		if rest == "" {
			return "$HOME"
		}
		return "$HOME/" + shellQuote(rest)
	}
	return shellQuote(p)
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
