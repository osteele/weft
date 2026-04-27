package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// sourceCacheDir is where we keep the most recent source tarball for each
// RemoteDir, so the agent can re-extract on demand if the working directory
// is found empty/incomplete when a job starts. Overridable for tests.
var sourceCacheDir = "/var/cache/weft-sources"

// sourceRegistry maps a RemoteDir (the on-rental absolute path of a project's
// working directory) to the R2 key of the most recently applied source
// tarball for that directory. It is populated by applySourceUpdate and
// consulted by ensureSourceFresh before each job runs.
//
// The registry lets the agent recover from a missing/partial workdir without
// requiring the controller to re-send the job request. The original failure
// mode this guards against: a previous campaign-mid eager-cleanup race
// (now fixed) deleted the workdir between checkForNewJobs polls. Even with
// that race fixed, the on-rental disk can still go missing for other reasons
// (manual rm, a buggy job, an interrupted source extract), and a one-line
// warning + re-extract is a much better failure mode than a silent
// ModuleNotFoundError.
type sourceRegistry struct {
	mu     sync.Mutex
	latest map[string]string // RemoteDir → R2 key
}

var sources = &sourceRegistry{latest: map[string]string{}}

func (r *sourceRegistry) record(remoteDir, r2Key string) {
	if remoteDir == "" || r2Key == "" {
		return
	}
	r.mu.Lock()
	r.latest[remoteDir] = r2Key
	r.mu.Unlock()
}

func (r *sourceRegistry) lookup(remoteDir string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.latest[remoteDir]
	return key, ok
}

// sourceCachePath returns the stable on-disk path under sourceCacheDir for
// a given R2 key. Hashing the key avoids slash-in-filename issues.
func sourceCachePath(r2Key string) string {
	h := sha256.Sum256([]byte(r2Key))
	return filepath.Join(sourceCacheDir, hex.EncodeToString(h[:])+".tar.gz")
}

// hasSourceMarkers reports whether dir looks like a populated project
// working directory. Used by ensureSourceFresh to decide whether re-extract
// is needed.
//
// "Populated" means either a recognized project manifest is present, or the
// directory exists and is non-empty. We deliberately accept non-Python
// projects; the bug we are guarding against is a missing/wiped workdir, not
// a misconfigured project.
func hasSourceMarkers(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	for _, m := range []string{"pyproject.toml", "package.json", "Cargo.toml", "go.mod", "requirements.txt", "setup.py"} {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// ensureSourceFresh re-extracts the cached source tarball for jobDir if the
// directory has gone missing or empty since it was first staged. Returns nil
// when no recovery is needed or when no cache entry exists for jobDir
// (in which case the caller will see whatever state the disk is in).
//
// All failures are best-effort: re-extract is a recovery path, not a hard
// dependency, so we log and return rather than blocking the job.
func ensureSourceFresh(bucket, jobDir string) {
	if jobDir == "" {
		return
	}
	if hasSourceMarkers(jobDir) {
		return
	}
	r2Key, ok := sources.lookup(jobDir)
	if !ok {
		slog.Warn("workdir is empty/missing and no source tarball is registered; job will run against bare disk",
			"component", "agent", "workdir", jobDir)
		return
	}

	cachePath := sourceCachePath(r2Key)
	if _, err := os.Stat(cachePath); err != nil {
		slog.Warn("workdir empty; re-downloading source tarball from R2",
			"component", "agent", "workdir", jobDir, "r2_key", r2Key)
		if err := downloadSourceToCache(bucket, r2Key, cachePath); err != nil {
			slog.Warn("source re-download failed; continuing without recovery",
				"component", "agent", "workdir", jobDir, "r2_key", r2Key, "error", err)
			return
		}
	} else {
		slog.Warn("workdir empty; re-extracting cached source tarball",
			"component", "agent", "workdir", jobDir, "r2_key", r2Key, "cache_path", cachePath)
	}

	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		slog.Warn("source re-stage: mkdir failed",
			"component", "agent", "workdir", jobDir, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tar", "xzf", cachePath, "-C", jobDir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		slog.Warn("source re-stage: extract failed",
			"component", "agent", "workdir", jobDir, "cache_path", cachePath, "error", err)
	}
}

func downloadSourceToCache(bucket, r2Key, cachePath string) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "copyto", fmt.Sprintf("r2:%s/%s", bucket, r2Key), cachePath)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
