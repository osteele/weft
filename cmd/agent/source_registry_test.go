package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/opsqueue"
)

func TestSourceRegistry_RecordAndLookup(t *testing.T) {
	r := &sourceRegistry{latest: map[string]registeredSource{}}
	r.record("/workspace/proj-a", "sources/abc.tar.gz")
	r.record("/workspace/proj-b", "sources/def.tar.gz")

	if got, ok := r.lookup("/workspace/proj-a"); !ok || got != "sources/abc.tar.gz" {
		t.Errorf("lookup proj-a = (%q, %v), want (sources/abc.tar.gz, true)", got, ok)
	}
	if _, ok := r.lookup("/workspace/missing"); ok {
		t.Errorf("lookup of unregistered dir returned ok=true")
	}

	r.record("/workspace/proj-a", "sources/abc-v2.tar.gz") // overwrite
	if got, _ := r.lookup("/workspace/proj-a"); got != "sources/abc-v2.tar.gz" {
		t.Errorf("after overwrite: lookup = %q, want sources/abc-v2.tar.gz", got)
	}

	// Empty inputs are ignored.
	r.record("", "x")
	r.record("/y", "")
	if _, ok := r.lookup(""); ok {
		t.Errorf("empty key was recorded")
	}
}

func TestHasSourceMarkers(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if hasSourceMarkers(missing) {
		t.Errorf("missing dir reported as populated")
	}

	empty := t.TempDir()
	if hasSourceMarkers(empty) {
		t.Errorf("empty dir reported as populated")
	}

	withFile := t.TempDir()
	if err := os.WriteFile(filepath.Join(withFile, "random.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !hasSourceMarkers(withFile) {
		t.Errorf("non-empty dir reported as missing")
	}

	withMarker := t.TempDir()
	if err := os.WriteFile(filepath.Join(withMarker, "pyproject.toml"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !hasSourceMarkers(withMarker) {
		t.Errorf("dir with pyproject.toml reported as missing")
	}
}

func TestSourceCachePath_Stable(t *testing.T) {
	a := sourceCachePath("sources/projects/foo/bar.tar.gz")
	b := sourceCachePath("sources/projects/foo/bar.tar.gz")
	c := sourceCachePath("sources/projects/foo/bar2.tar.gz")
	if a != b {
		t.Errorf("same key produced different paths: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different keys produced same path")
	}
	if filepath.Dir(a) != sourceCacheDir() {
		t.Errorf("cache path not under sourceCacheDir: %q", a)
	}
}

func TestFetchSourceManifestMaterializesPinnedRootsAndBlobs(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "project.tar.gz", map[string]string{"version.txt": "submitted\n"})
	writeSourceTarball(t, objectDir, "sibling.tar.gz", map[string]string{"lib.txt": "sibling\n"})
	blobContent := "immutable payload\n"
	blobHash := contentSHA256(blobContent)
	writeFakeR2Object(t, objectDir, "assets/"+blobHash, blobContent)
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	roots := []opsqueue.SourceRoot{
		{
			MountBasename: "project",
			Hash:          canonicalTarballSHA256(t, filepath.Join(objectDir, "project.tar.gz")),
			R2Key:         "sources/project.tar.gz",
			Blobs: []dataplane.SourceBlob{{
				R2Key: "assets/" + blobHash, RelPath: "data/input.bin", SHA256: blobHash,
			}},
		},
		{
			MountBasename: "sibling",
			Hash:          canonicalTarballSHA256(t, filepath.Join(objectDir, "sibling.tar.gz")),
			R2Key:         "sources/sibling.tar.gz",
		},
	}
	manifest := opsqueue.SourceManifest{SHA256: queueSourceManifestSHA256(t, roots), Roots: roots}
	perJobRoot := filepath.Join(t.TempDir(), "source")
	projectDir, err := fetchSourceManifestToDir("test-bucket", manifest, perJobRoot)
	if err != nil {
		t.Fatalf("fetchSourceManifestToDir: %v", err)
	}
	if projectDir != filepath.Join(perJobRoot, "project") {
		t.Fatalf("project dir = %q", projectDir)
	}
	checks := map[string]string{
		filepath.Join(projectDir, "version.txt"):        "submitted\n",
		filepath.Join(projectDir, "data", "input.bin"):  blobContent,
		filepath.Join(perJobRoot, "sibling", "lib.txt"): "sibling\n",
	}
	for filename, want := range checks {
		got, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read %s: %v", filename, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", filename, got, want)
		}
	}
}

func TestFetchSourceManifestRejectsTamperedRoot(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "project.tar.gz", map[string]string{"version.txt": "tampered\n"})
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	roots := []opsqueue.SourceRoot{{
		MountBasename: "project",
		Hash:          strings.Repeat("a", 64),
		R2Key:         "sources/project.tar.gz",
	}}
	manifest := opsqueue.SourceManifest{SHA256: queueSourceManifestSHA256(t, roots), Roots: roots}
	_, err := fetchSourceManifestToDir("test-bucket", manifest, filepath.Join(t.TempDir(), "source"))
	if err == nil || !strings.Contains(err.Error(), "canonical tar SHA-256") {
		t.Fatalf("error = %v, want canonical tar SHA mismatch", err)
	}
}

func canonicalTarballSHA256(t *testing.T, filename string) string {
	t.Helper()
	f, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, gr); err != nil {
		t.Fatal(err)
	}
	if err := gr.Close(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func queueSourceManifestSHA256(t *testing.T, roots []opsqueue.SourceRoot) string {
	t.Helper()
	data, err := dataplane.SourceManifestSHA256(roots)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEnsureSourceFreshMountsRepairsMissingSibling(t *testing.T) {
	cacheDir := t.TempDir()
	oldCacheDir := sourceCacheDirOverride
	sourceCacheDirOverride = cacheDir
	t.Cleanup(func() { sourceCacheDirOverride = oldCacheDir })

	oldSources := sources
	sources = &sourceRegistry{latest: map[string]registeredSource{}}
	t.Cleanup(func() { sources = oldSources })

	parent := t.TempDir()
	projectDir := filepath.Join(parent, "project")
	siblingDir := filepath.Join(parent, "research-expkit")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "pyproject.toml"), []byte("[project]\nname='project'\n"), 0o644); err != nil {
		t.Fatalf("write project marker: %v", err)
	}

	projectKey := "sources/project.tar.gz"
	siblingKey := "sources/research-expkit.tar.gz"
	registerSourceMounts([]cloud.SourceMount{
		{RemoteDir: projectDir, R2Key: projectKey},
		{RemoteDir: siblingDir, R2Key: siblingKey},
	})
	writeTestTarGz(t, sourceCachePath(siblingKey), map[string]string{
		"expkit/__init__.py": "VALUE = 1\n",
	})

	if err := ensureSourceFreshMounts("bucket", []cloud.SourceMount{
		{RemoteDir: projectDir, R2Key: projectKey},
		{RemoteDir: siblingDir, R2Key: siblingKey},
	}); err != nil {
		t.Fatalf("ensureSourceFreshMounts: %v", err)
	}

	if _, err := os.Stat(filepath.Join(siblingDir, "expkit", "__init__.py")); err != nil {
		t.Fatalf("sibling root was not repaired: %v", err)
	}
}

func TestEnsureSourceFreshMountsRestoresBlobs(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "source.tar.gz", map[string]string{"main.py": "print('ok')\n"})
	blobContent := "restored data\n"
	blobHash := contentSHA256(blobContent)
	blobKey := "assets/" + blobHash
	writeFakeR2Object(t, objectDir, blobKey, blobContent)
	invocationLog := filepath.Join(t.TempDir(), "rclone.log")
	t.Setenv("RCLONE_INVOCATION_LOG", invocationLog)
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	update := controlplane.SourceUpdate{
		RemoteDir: remoteDir,
		R2Key:     "sources/source.tar.gz",
		Blobs: []controlplane.SourceBlob{{
			R2Key: blobKey, RelPath: "data/input.bin", SHA256: blobHash,
		}},
	}
	if err := applySourceUpdate("test-bucket", update); err != nil {
		t.Fatalf("applySourceUpdate: %v", err)
	}
	if err := os.RemoveAll(remoteDir); err != nil {
		t.Fatalf("remove source tree: %v", err)
	}
	if err := ensureSourceFreshMounts("test-bucket", []cloud.SourceMount{{RemoteDir: remoteDir, R2Key: update.R2Key}}); err != nil {
		t.Fatalf("ensureSourceFreshMounts: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(remoteDir, "data", "input.bin"))
	if err != nil {
		t.Fatalf("read restored blob: %v", err)
	}
	if string(got) != blobContent {
		t.Fatalf("restored blob = %q, want %q", got, blobContent)
	}
	data, err := os.ReadFile(invocationLog)
	if err != nil {
		t.Fatalf("read rclone invocation log after cache hit: %v", err)
	}
	if batchCopies := countBlobBatchCopies(data); batchCopies != 1 {
		t.Fatalf("blob batch download count after cache-hit recovery = %d, want 1; invocations:\n%s", batchCopies, data)
	}

	if err := os.RemoveAll(remoteDir); err != nil {
		t.Fatalf("remove source tree before cache-miss recovery: %v", err)
	}
	if err := os.Remove(sourceBlobCachePath(blobHash)); err != nil {
		t.Fatalf("remove cached blob: %v", err)
	}
	if err := ensureSourceFreshMounts("test-bucket", []cloud.SourceMount{{RemoteDir: remoteDir, R2Key: update.R2Key}}); err != nil {
		t.Fatalf("ensureSourceFreshMounts after cache removal: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(remoteDir, "data", "input.bin"))
	if err != nil {
		t.Fatalf("read re-downloaded blob: %v", err)
	}
	if string(got) != blobContent {
		t.Fatalf("re-downloaded blob = %q, want %q", got, blobContent)
	}
	data, err = os.ReadFile(invocationLog)
	if err != nil {
		t.Fatalf("read rclone invocation log: %v", err)
	}
	if batchCopies := countBlobBatchCopies(data); batchCopies != 2 {
		t.Fatalf("blob batch download count after cache-miss recovery = %d, want 2; invocations:\n%s", batchCopies, data)
	}
}

func TestEnsureSourceFreshMountsSwitchesToJobPinnedSnapshot(t *testing.T) {
	objectDir := t.TempDir()
	writeSourceTarball(t, objectDir, "job-a.tar.gz", map[string]string{"version.txt": "job A\n"})
	writeSourceTarball(t, objectDir, "job-b.tar.gz", map[string]string{"version.txt": "job B\n"})
	installFakeRcloneForSourceTarballs(t, objectDir)
	resetSourceUpdateState(t)

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	if err := applySourceUpdate("test-bucket", controlplane.SourceUpdate{
		RemoteDir: remoteDir,
		R2Key:     "sources/job-b.tar.gz",
	}); err != nil {
		t.Fatalf("install dispatch-time current tree: %v", err)
	}
	if err := ensureSourceFreshMounts("test-bucket", []cloud.SourceMount{{
		RemoteDir: remoteDir,
		R2Key:     "sources/job-a.tar.gz",
	}}); err != nil {
		t.Fatalf("switch to job A pin: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(remoteDir, "version.txt"))
	if err != nil {
		t.Fatalf("read switched source: %v", err)
	}
	if string(got) != "job A\n" {
		t.Fatalf("version.txt = %q, want job A snapshot", got)
	}
	registered, ok := sources.lookupSource(remoteDir)
	if !ok || registered.r2Key != "sources/job-a.tar.gz" {
		t.Fatalf("registered source = %#v, ok=%v", registered, ok)
	}
}

func countBlobBatchCopies(data []byte) int {
	batchCopies := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "copy ") {
			batchCopies++
		}
	}
	return batchCopies
}

func TestEnsureSourceFreshMountsWithoutRegistryIsHardError(t *testing.T) {
	resetSourceUpdateState(t)
	remoteDir := filepath.Join(t.TempDir(), "workspace", "missing-project")
	err := ensureSourceFreshMounts("test-bucket", []cloud.SourceMount{{RemoteDir: remoteDir, R2Key: "sources/source.tar.gz"}})
	if err == nil {
		t.Fatal("ensureSourceFreshMounts returned nil without registry state")
	}
	if !strings.Contains(err.Error(), remoteDir) || !strings.Contains(err.Error(), "no registered source payload") {
		t.Fatalf("error = %q, want mount path and missing registry state", err)
	}
}

func writeTestTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir cache parent: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tarball: %v", err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := io.WriteString(tw, content); err != nil {
			t.Fatalf("write tar content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tarball: %v", err)
	}
}

// TestResolveSourceCacheDir_UserPathForNonRoot verifies the regression that
// stranded wj2301 on cool30: the R2-isolated source fallback failed with
// "permission denied" because the agent (running as a regular user) tried to
// mkdir /var/cache/weft-sources. Non-root invocations must pick a
// user-writable path under XDG_CACHE_HOME or $HOME/.cache.
func TestResolveSourceCacheDir_UserPathForNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test must run as non-root to exercise the user-cache path")
	}

	// Prefer XDG_CACHE_HOME when set.
	xdgDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdgDir)
	got := resolveSourceCacheDir()
	want := filepath.Join(xdgDir, "weft-sources")
	if got != want {
		t.Errorf("with XDG_CACHE_HOME=%q: got %q, want %q", xdgDir, got, want)
	}

	// Fall back to $HOME/.cache when XDG_CACHE_HOME is unset.
	t.Setenv("XDG_CACHE_HOME", "")
	got = resolveSourceCacheDir()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("UserHomeDir unavailable; cannot exercise HOME fallback")
	}
	want = filepath.Join(home, ".cache", "weft-sources")
	if got != want {
		t.Errorf("with XDG_CACHE_HOME unset: got %q, want %q", got, want)
	}

	// The chosen directory must be writable by the agent so the R2-isolated
	// fallback path can mkdir it on demand.
	if err := os.MkdirAll(got, 0o755); err != nil {
		t.Errorf("resolved source cache dir %q is not creatable: %v", got, err)
	}
}

// TestResolveSourceCacheDir_HomelessFallsBackToTempDir verifies the second
// half of the regression: a non-root process with neither XDG_CACHE_HOME nor
// a usable HOME (e.g. a systemd unit without Environment=HOME=, a stripped-env
// container) must NOT fall back to /var/cache/weft-sources — that is the exact
// path that produced the original mkdir-permission-denied wedge.
func TestResolveSourceCacheDir_HomelessFallsBackToTempDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test must run as non-root to exercise the user-cache path")
	}
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	got := resolveSourceCacheDir()
	if got == "/var/cache/weft-sources" {
		t.Fatalf("HOME-less non-root must not fall back to /var/cache (the original bug); got %q", got)
	}
	want := filepath.Join(os.TempDir(), "weft-sources")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSourceCacheDir_LazyHonorsEnvMutation verifies the cache directory is
// resolved on each call (not frozen at package init), so privilege drops or
// late env mutations (including t.Setenv in tests) take effect.
func TestSourceCacheDir_LazyHonorsEnvMutation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test must run as non-root")
	}
	first := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", first)
	got1 := sourceCacheDir()
	if got1 != filepath.Join(first, "weft-sources") {
		t.Fatalf("first call: got %q, want %q", got1, filepath.Join(first, "weft-sources"))
	}

	second := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", second)
	got2 := sourceCacheDir()
	if got2 != filepath.Join(second, "weft-sources") {
		t.Errorf("after env mutation: got %q, want %q", got2, filepath.Join(second, "weft-sources"))
	}
}
