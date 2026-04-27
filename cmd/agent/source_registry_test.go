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
	if filepath.Dir(a) != sourceCacheDir {
		t.Errorf("cache path not under sourceCacheDir: %q", a)
	}
}
