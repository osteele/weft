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
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

// sourceCacheDir returns the directory where the agent keeps the most recent
// source tarball for each RemoteDir, so it can re-extract on demand if a
// job's working directory is found empty/incomplete at start. Overridable
// for tests via sourceCacheDirOverride.
//
// Default selection (resolveSourceCacheDir): cloud rentals run the agent as
// root and use /var/cache/weft-sources; on-prem hosts run as a regular user
// and use ${XDG_CACHE_HOME:-$HOME/.cache}/weft-sources. Non-root processes
// without a usable HOME fall back to os.TempDir()/weft-sources rather than
// /var/cache (which is the original permission-denied bug). Resolution is
// lazy so privilege drops between init and first use, and tests that
// t.Setenv HOME/XDG_CACHE_HOME, are honored.
var sourceCacheDirOverride string

func sourceCacheDir() string {
	if sourceCacheDirOverride != "" {
		return sourceCacheDirOverride
	}
	return resolveSourceCacheDir()
}

// resolveSourceCacheDir picks a writable default for the source tarball
// cache. Root processes (typical on cloud rentals) get the system path;
// other uids get a user-cache path so the agent can mkdir without sudo.
// When XDG_CACHE_HOME is unset and UserHomeDir fails (HOME-less daemons,
// stripped-env containers), fall back to a TempDir path the current uid
// can definitely write — never /var/cache, which is the configuration
// that originally produced the permission-denied wedge.
func resolveSourceCacheDir() string {
	const systemPath = "/var/cache/weft-sources"
	if os.Geteuid() == 0 {
		return systemPath
	}
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "weft-sources")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "weft-sources")
	}
	return filepath.Join(os.TempDir(), "weft-sources")
}

// sourceRegistry maps a RemoteDir (the on-rental absolute path of a project's
// working directory) to the R2 key of the most recently applied source
// tarball for that directory. It is populated by applySourceUpdate and
// consulted by ensureSourceFreshMounts before each job runs.
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

type activeSourceWorkdirTracker struct {
	mu    sync.Mutex
	users map[string]map[int64]int
}

var activeSourceWorkdirs = &activeSourceWorkdirTracker{users: map[string]map[int64]int{}}

func (t *activeSourceWorkdirTracker) begin(jobID int64, workdir string) func() {
	if strings.TrimSpace(workdir) == "" || jobID <= 0 {
		return func() {}
	}
	dir := filepath.Clean(workdir)
	t.mu.Lock()
	if t.users == nil {
		t.users = map[string]map[int64]int{}
	}
	jobs := t.users[dir]
	if jobs == nil {
		jobs = map[int64]int{}
		t.users[dir] = jobs
	}
	jobs[jobID]++
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			jobs := t.users[dir]
			if jobs == nil {
				return
			}
			if jobs[jobID] <= 1 {
				delete(jobs, jobID)
			} else {
				jobs[jobID]--
			}
			if len(jobs) == 0 {
				delete(t.users, dir)
			}
		})
	}
}

// beginMounts holds every source root a job reads. Returns one release that
// frees all of them; a job with uv path deps reads sibling roots as well as
// its working directory, and a source update to any of them mid-run is the
// same hazard.
func (t *activeSourceWorkdirTracker) beginMounts(jobID int64, mounts []cloud.SourceMount) func() {
	releases := make([]func(), 0, len(mounts))
	for _, mount := range mounts {
		releases = append(releases, t.begin(jobID, mount.RemoteDir))
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, release := range releases {
				release()
			}
		})
	}
}

func (t *activeSourceWorkdirTracker) runningJob(workdir string) (int64, bool) {
	if strings.TrimSpace(workdir) == "" {
		return 0, false
	}
	dir := filepath.Clean(workdir)
	t.mu.Lock()
	defer t.mu.Unlock()
	jobs := t.users[dir]
	if len(jobs) == 0 {
		return 0, false
	}
	var id int64
	for jobID := range jobs {
		if id == 0 || jobID < id {
			id = jobID
		}
	}
	return id, true
}

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
	return filepath.Join(sourceCacheDir(), hex.EncodeToString(h[:])+".tar.gz")
}

// hasSourceMarkers reports whether dir looks like a populated project
// working directory. Used by ensureSourceFreshMounts to decide whether re-extract
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

// ensureSourceFreshMounts re-extracts the cached source tarball for each of a
// job's source mounts whose directory has gone missing or empty since it was
// first staged. Every mount is tested, not just the project root: a populated
// project root does not imply a populated sibling root, and a missing sibling
// otherwise surfaces only as an import error inside the job (spec:
// source-data-sync.allium § StageSourceOnCloudInstance).
//
// All failures are best-effort: re-extract is a recovery path, not a hard
// dependency, so we log and return rather than blocking the job.
func ensureSourceFreshMounts(bucket string, mounts []cloud.SourceMount) {
	if len(mounts) == 0 {
		return
	}
	for _, mount := range mounts {
		ensureOneSourceFresh(bucket, mount.RemoteDir)
	}
}

func registerSourceMounts(mounts []cloud.SourceMount) {
	for _, mount := range mounts {
		sources.record(mount.RemoteDir, mount.R2Key)
	}
}

func expandedSourceMountsForJob(job cloud.AgentJob) []cloud.SourceMount {
	if len(job.SourceMounts) > 0 {
		mounts := append([]cloud.SourceMount(nil), job.SourceMounts...)
		for i := range mounts {
			mounts[i].RemoteDir = runnerExpandTilde(mounts[i].RemoteDir)
		}
		return mounts
	}
	if job.Dir == "" {
		return nil
	}
	return []cloud.SourceMount{{RemoteDir: runnerExpandTilde(job.Dir)}}
}

func runnerExpandTilde(p string) string {
	if len(p) > 1 && p[0] == '~' {
		home, _ := os.UserHomeDir()
		return home + p[1:]
	}
	return p
}

func ensureOneSourceFresh(bucket, jobDir string) {
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

// fetchSourceTarballToDir downloads sources/<sha>.tar.gz from R2 (or reuses a
// cached copy) and extracts it into perJobDir. Used as the implementation of
// runner.Runner.EnsureSourceFromR2: returns a non-nil error on any failure so
// the runner can reject the attempt at preflight instead of silently running
// against the wrong sources.
//
// Unlike ensureSourceFreshMounts, this is a primary path (not a best-effort
// recovery), so errors are propagated rather than logged-and-swallowed. The
// per-job dir is created if missing and the cached tarball is shared across
// jobs that need the same R2 key (content-addressed).
func fetchSourceTarballToDir(bucket, r2Key, perJobDir string) error {
	if bucket == "" {
		return fmt.Errorf("no R2 bucket configured")
	}
	if r2Key == "" {
		return fmt.Errorf("empty r2 key")
	}
	if perJobDir == "" {
		return fmt.Errorf("empty per-job dir")
	}
	cachePath := sourceCachePath(r2Key)
	if _, err := os.Stat(cachePath); err != nil {
		if err := downloadSourceToCache(bucket, r2Key, cachePath); err != nil {
			return fmt.Errorf("download tarball: %w", err)
		}
	}
	// Clean any partial leftovers from a prior interrupted extract so the
	// new extract isn't a merge of two trees. Cheap insurance — the per-job
	// dir is single-use.
	if err := os.RemoveAll(perJobDir); err != nil {
		return fmt.Errorf("clean per-job dir: %w", err)
	}
	if err := os.MkdirAll(perJobDir, 0o755); err != nil {
		return fmt.Errorf("mkdir per-job dir: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tar", "xzf", cachePath, "-C", perJobDir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extract tarball: %w", err)
	}
	// Record the mapping so any subsequent ensureSourceFreshMounts recovery (e.g.
	// a kill+resubmit during a grace window) can find the tarball again.
	sources.record(perJobDir, r2Key)
	slog.Info("staged R2-isolated source", "component", "agent", "per_job_dir", perJobDir, "r2_key", r2Key)
	return nil
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
