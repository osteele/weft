package ops

import (
	"database/sql"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ssh"
)

var snapshotRemoteJobOutputFunc = snapshotRemoteJobOutput

// RecordJobOutputs registers a completed job's declared outputs — `--output`
// refs (asset refs and `local:` paths) plus `--produces` manifest entries —
// as data assets on the host where the job ran. Filesystem outputs are
// snapshotted into the host's per-run artifact directory first, so the
// recorded path stays valid after the working directory is reused; the
// snapshot needs no R2 credentials. Recording also makes outputs immediately
// visible for transfer cost estimation and pre-staging of downstream jobs.
//
// Only records outputs for successfully completed jobs (exit code 0).
// Silently does nothing if the job declares no outputs or didn't succeed.
func RecordJobOutputs(database *sql.DB, job *db.Job) {
	if job == nil {
		return
	}
	records := declaredOutputRecords(job)
	if len(records) == 0 {
		return
	}
	// Only record on success
	if job.Status != db.StatusCompleted || job.ExitCode == nil || *job.ExitCode != 0 {
		return
	}

	now := time.Now()
	recorded := 0
	for _, rec := range records {
		recordPath := rec.path
		if rec.path != "" {
			snapshotPath, err := snapshotRemoteJobOutputFunc(database, job, rec.path, defaultSourceSyncTimeout)
			if err != nil {
				oplog.LogJob("job.record-outputs", job.ID, job.Host,
					oplog.WithDetailf("output snapshot failed for %s: %v", rec.path, err))
				continue
			}
			recordPath = snapshotPath
		}
		entry := dataloc.HostDataEntry{
			Host:     job.Host,
			Asset:    rec.asset,
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
			oplog.WithDetailf("%d/%d outputs recorded", recorded, len(records)))
	}
}

// declaredOutputRecord pairs the data asset to record with the working-dir
// relative path to snapshot ("" for non-filesystem asset refs).
type declaredOutputRecord struct {
	asset dataloc.DataAsset
	path  string
}

// declaredOutputRecords enumerates a job's declared outputs: `--output`
// refs and `--produces` manifest entries. Produces specs live on the job
// record, so hosts without R2 can snapshot them at completion without
// reading the remote manifest. Paths declared both ways are deduplicated.
func declaredOutputRecords(job *db.Job) []declaredOutputRecord {
	var records []declaredOutputRecord
	seenPaths := make(map[string]struct{})
	add := func(asset dataloc.DataAsset, path string) {
		if path != "" {
			if _, dup := seenPaths[path]; dup {
				return
			}
			seenPaths[path] = struct{}{}
		}
		records = append(records, declaredOutputRecord{asset: asset, path: path})
	}
	for _, ref := range job.Outputs {
		if asset, outputPath, ok := parseRecordedOutputRef(job.ID, ref); ok {
			add(asset, outputPath)
		}
	}
	for _, raw := range job.Produces {
		if asset, outputPath, ok := parseProducedOutputRef(job.ID, raw); ok {
			add(asset, outputPath)
		}
	}
	return records
}

// parseProducedOutputRef maps a --produces spec ("output/model.pt" or
// "output/model.pt:<version>") onto the same job-output asset form as a
// `local:` declared output. The path/version split mirrors
// runner.ParseProducesSpec, which ops cannot import (cycle via placement).
func parseProducedOutputRef(jobID int64, raw string) (dataloc.DataAsset, string, bool) {
	path := raw
	if idx := strings.LastIndex(raw, ":"); idx >= 0 {
		if _, err := strconv.ParseInt(raw[idx+1:], 10, 64); err == nil {
			path = raw[:idx]
		}
	}
	relPath := filepath.Clean(strings.TrimSpace(path))
	if relPath == "" || relPath == "." {
		return dataloc.DataAsset{}, "", false
	}
	return dataloc.DataAsset{
		Kind: dataloc.AssetJobOutput,
		ID:   fmt.Sprintf("%d/%s", jobID, relPath),
	}, relPath, true
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
