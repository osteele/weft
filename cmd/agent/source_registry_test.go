package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceRegistry_RecordAndLookup(t *testing.T) {
	r := &sourceRegistry{latest: map[string]string{}}
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
