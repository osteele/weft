package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/opsqueue"
	srcsync "github.com/osteele/weft/internal/sync"
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
// working directory) to the most recently applied source payload for that
// directory. It is populated by applySourceUpdate and consulted by
// ensureSourceFreshMounts before each job runs.
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
	latest map[string]registeredSource // RemoteDir → source payload
}

type registeredSource struct {
	r2Key string
	blobs []controlplane.SourceBlob
}

var sources = &sourceRegistry{latest: map[string]registeredSource{}}

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
	r.recordWithBlobs(remoteDir, r2Key, nil)
}

func (r *sourceRegistry) recordWithBlobs(remoteDir, r2Key string, blobs []controlplane.SourceBlob) {
	if remoteDir == "" || r2Key == "" {
		return
	}
	r.mu.Lock()
	r.latest[remoteDir] = registeredSource{
		r2Key: r2Key,
		blobs: append([]controlplane.SourceBlob(nil), blobs...),
	}
	r.mu.Unlock()
}

func (r *sourceRegistry) lookup(remoteDir string) (string, bool) {
	source, ok := r.lookupSource(remoteDir)
	return source.r2Key, ok
}

func (r *sourceRegistry) lookupSource(remoteDir string) (registeredSource, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	source, ok := r.latest[remoteDir]
	source.blobs = append([]controlplane.SourceBlob(nil), source.blobs...)
	return source, ok
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

// ensureSourceFreshMounts reconstructs each empty or missing source mount.
// Every mount is tested, not just the project root: a populated project root
// does not imply a populated sibling root, and a missing sibling otherwise
// surfaces only as an import error inside the job (spec:
// source-data-sync.allium § StageSourceOnCloudInstance).
func ensureSourceFreshMounts(bucket string, mounts []cloud.SourceMount) error {
	if len(mounts) == 0 {
		return nil
	}
	for _, mount := range mounts {
		if err := ensureOneSourceFresh(bucket, mount); err != nil {
			return fmt.Errorf("restore source mount %s: %w", mount.RemoteDir, err)
		}
	}
	return nil
}

func registerSourceMounts(mounts []cloud.SourceMount) {
	for _, mount := range mounts {
		sources.recordWithBlobs(mount.RemoteDir, mount.R2Key, mount.Blobs)
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

func ensureOneSourceFresh(bucket string, mount cloud.SourceMount) error {
	jobDir := mount.RemoteDir
	if jobDir == "" {
		return nil
	}
	source, ok := sources.lookupSource(jobDir)
	if !ok {
		return fmt.Errorf("empty or missing source directory has no registered source payload")
	}
	blobsDiffer := mount.Blobs != nil && !slices.Equal(source.blobs, mount.Blobs)
	if mount.R2Key != "" && (source.r2Key != mount.R2Key || blobsDiffer) {
		return applySourceUpdate(bucket, controlplane.SourceUpdate{
			RemoteDir: jobDir,
			R2Key:     mount.R2Key,
			Blobs:     mount.Blobs,
		})
	}
	if hasSourceMarkers(jobDir) {
		return nil
	}

	cachePath := sourceCachePath(source.r2Key)
	if _, err := os.Stat(cachePath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect cached source tarball %s: %w", source.r2Key, err)
		}
		slog.Warn("workdir empty; re-downloading source tarball from R2",
			"component", "agent", "workdir", jobDir, "r2_key", source.r2Key)
		if err := downloadSourceToCache(bucket, source.r2Key, cachePath); err != nil {
			return fmt.Errorf("download source tarball %s: %w", source.r2Key, err)
		}
	} else {
		slog.Warn("workdir empty; re-extracting cached source tarball",
			"component", "agent", "workdir", jobDir, "r2_key", source.r2Key, "cache_path", cachePath)
	}
	if err := ensureSourceBlobsCached(bucket, source.blobs); err != nil {
		return fmt.Errorf("restore source blobs: %w", err)
	}

	if err := os.RemoveAll(jobDir); err != nil {
		return fmt.Errorf("clean source directory: %w", err)
	}
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return fmt.Errorf("create source directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tar", "xzf", cachePath, "-C", jobDir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extract source tarball %s: %w", source.r2Key, err)
	}
	if err := materializeSourceBlobs(jobDir, source.blobs); err != nil {
		return fmt.Errorf("materialize restored source blobs: %w", err)
	}
	return nil
}

// fetchSourceTarballToDir downloads the exact recorded v1 or v2 source key
// from R2 (or reuses a cached copy) and extracts it into perJobDir. Used as the
// implementation of runner.Runner.EnsureSourceFromR2: returns a non-nil error
// on any failure so the runner can reject the attempt at preflight instead of
// silently running against the wrong sources.
//
// The per-job dir is created if missing and the cached tarball is shared
// across jobs that need the same R2 key (content-addressed).
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

// fetchSourceManifestToDir materializes every root in an immutable source
// closure under one per-job parent. The first root is the project working
// directory; later roots are adjacent siblings, preserving ../path imports.
func fetchSourceManifestToDir(bucket string, manifest opsqueue.SourceManifest, perJobRoot string) (string, error) {
	if bucket == "" {
		return "", fmt.Errorf("no R2 bucket configured")
	}
	if perJobRoot == "" {
		return "", fmt.Errorf("empty per-job source root")
	}
	if err := validateQueueSourceManifest(manifest); err != nil {
		return "", err
	}

	for _, root := range manifest.Roots {
		if err := ensurePinnedSourceTarballCached(bucket, root); err != nil {
			return "", fmt.Errorf("prepare source root %s: %w", root.MountBasename, err)
		}
	}
	var blobs []controlplane.SourceBlob
	for _, root := range manifest.Roots {
		blobs = append(blobs, root.Blobs...)
	}
	if err := ensureSourceBlobsCached(bucket, blobs); err != nil {
		return "", fmt.Errorf("prepare source blobs: %w", err)
	}

	if err := os.RemoveAll(perJobRoot); err != nil {
		return "", fmt.Errorf("clean per-job source root: %w", err)
	}
	if err := os.MkdirAll(perJobRoot, 0o755); err != nil {
		return "", fmt.Errorf("create per-job source root: %w", err)
	}
	for _, root := range manifest.Roots {
		rootDir := filepath.Join(perJobRoot, root.MountBasename)
		if err := os.MkdirAll(rootDir, 0o755); err != nil {
			return "", fmt.Errorf("create source root %s: %w", root.MountBasename, err)
		}
		if err := srcsync.ExtractTarball(sourceCachePath(root.R2Key), rootDir); err != nil {
			return "", fmt.Errorf("extract source root %s: %w", root.MountBasename, err)
		}
		if err := materializeSourceBlobs(rootDir, root.Blobs); err != nil {
			return "", fmt.Errorf("materialize source root %s blobs: %w", root.MountBasename, err)
		}
	}

	projectDir := filepath.Join(perJobRoot, manifest.Roots[0].MountBasename)
	slog.Info("staged pinned source manifest", "component", "agent", "per_job_dir", projectDir, "source_manifest_sha256", manifest.SHA256, "roots", len(manifest.Roots))
	return projectDir, nil
}

func validateQueueSourceManifest(manifest opsqueue.SourceManifest) error {
	manifestHash, err := normalizeSHA256(manifest.SHA256)
	if err != nil {
		return fmt.Errorf("source manifest: %w", err)
	}
	if len(manifest.Roots) == 0 {
		return fmt.Errorf("source manifest has no roots")
	}
	identities := make([]dataplane.SourceManifestRoot, 0, len(manifest.Roots))
	basenames := make(map[string]struct{}, len(manifest.Roots))
	for i, root := range manifest.Roots {
		if root.MountBasename == "" || root.MountBasename == "." || root.MountBasename == ".." || strings.ContainsAny(root.MountBasename, "/\\") {
			return fmt.Errorf("source root %d has unsafe mount basename %q", i, root.MountBasename)
		}
		if _, exists := basenames[root.MountBasename]; exists {
			return fmt.Errorf("source root %d duplicates mount basename %q", i, root.MountBasename)
		}
		basenames[root.MountBasename] = struct{}{}
		rootHash, err := normalizeSHA256(root.Hash)
		if err != nil {
			return fmt.Errorf("source root %d: %w", i, err)
		}
		if !safeSlashRelativePath(root.R2Key) {
			return fmt.Errorf("source root %d has unsafe r2_key %q", i, root.R2Key)
		}
		if _, err := validateSourceBlobs(root.Blobs); err != nil {
			return fmt.Errorf("source root %d blobs: %w", i, err)
		}
		identities = append(identities, dataplane.SourceManifestRoot{
			MountBasename: root.MountBasename,
			Hash:          rootHash,
			R2Key:         root.R2Key,
			Blobs:         root.Blobs,
		})
	}
	actual, err := dataplane.SourceManifestSHA256(identities)
	if err != nil {
		return err
	}
	if actual != manifestHash {
		return fmt.Errorf("source manifest SHA-256 %s does not match payload %s", manifestHash, actual)
	}
	return nil
}

func ensurePinnedSourceTarballCached(bucket string, root opsqueue.SourceRoot) error {
	cachePath := sourceCachePath(root.R2Key)
	if _, err := os.Stat(cachePath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect cache: %w", err)
		}
		if err := downloadSourceToCache(bucket, root.R2Key, cachePath); err != nil {
			return fmt.Errorf("download tarball: %w", err)
		}
	}
	if err := verifySourceTarballSHA256(cachePath, root.Hash); err != nil {
		// A partial prior download or local cache corruption is recoverable.
		// Fetch once more from the immutable object before rejecting the job.
		if removeErr := os.Remove(cachePath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("discard invalid cached tarball %s: %w", root.R2Key, removeErr)
		}
		if downloadErr := downloadSourceToCache(bucket, root.R2Key, cachePath); downloadErr != nil {
			return fmt.Errorf("redownload tarball %s after verification failure: %w", root.R2Key, downloadErr)
		}
		if retryErr := verifySourceTarballSHA256(cachePath, root.Hash); retryErr != nil {
			return fmt.Errorf("verify tarball %s after redownload: %w", root.R2Key, retryErr)
		}
	}
	return nil
}

func verifySourceTarballSHA256(filename, expected string) error {
	want, err := normalizeSHA256(expected)
	if err != nil {
		return err
	}
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, gr); err != nil {
		_ = gr.Close()
		return fmt.Errorf("hash canonical tar stream: %w", err)
	}
	if err := gr.Close(); err != nil {
		return fmt.Errorf("close gzip stream: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("canonical tar SHA-256 %s, want %s", got, want)
	}
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
