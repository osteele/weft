package estimate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockfileHash(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "uv.lock"), []byte("test content\n"), 0644)

	hashes := LockfileHash([]string{dir})
	if len(hashes) != 1 {
		t.Fatalf("expected 1 hash, got %d", len(hashes))
	}
	h := hashes[dir]
	if len(h) < 10 {
		t.Errorf("hash too short: %s", h)
	}
	if h[:7] != "sha256:" {
		t.Errorf("hash should start with sha256:, got %s", h)
	}

	// Deterministic
	hashes2 := LockfileHash([]string{dir})
	if hashes2[dir] != h {
		t.Errorf("hash not deterministic: %s != %s", hashes2[dir], h)
	}
}

func TestLockfileHash_MissingFile(t *testing.T) {
	dir := t.TempDir()
	hashes := LockfileHash([]string{dir})
	if len(hashes) != 0 {
		t.Errorf("expected 0 hashes for dir without uv.lock, got %d", len(hashes))
	}
}

func TestEstimateUVSyncBytes_SingleManifest(t *testing.T) {
	manifests := map[string]*UVManifestRef{
		"/proj/a": {
			Packages: []UVPackageRef{
				{Name: "numpy", Version: "1.26.4", SizeBytes: 1000},
				{Name: "torch", Version: "2.2.1", SizeBytes: 5000},
			},
		},
	}

	total := EstimateUVSyncBytes(manifests)
	if total != 6000 {
		t.Errorf("total = %d, want 6000", total)
	}
}

func TestEstimateUVSyncBytes_DeduplicatesSharedPackages(t *testing.T) {
	manifests := map[string]*UVManifestRef{
		"/proj/a": {
			Packages: []UVPackageRef{
				{Name: "numpy", Version: "1.26.4", SizeBytes: 1000},
				{Name: "torch", Version: "2.2.1", SizeBytes: 5000},
			},
		},
		"/proj/b": {
			Packages: []UVPackageRef{
				{Name: "numpy", Version: "1.26.4", SizeBytes: 1000},
				{Name: "pandas", Version: "2.1.0", SizeBytes: 2000},
			},
		},
	}

	total := EstimateUVSyncBytes(manifests)
	// numpy (1000) + torch (5000) + pandas (2000) = 8000
	// numpy is shared, counted only once
	if total != 8000 {
		t.Errorf("total = %d, want 8000", total)
	}
}

func TestEstimateUVSyncBytes_DifferentVersionsNotDeduplicated(t *testing.T) {
	manifests := map[string]*UVManifestRef{
		"/proj/a": {
			Packages: []UVPackageRef{
				{Name: "numpy", Version: "1.26.4", SizeBytes: 1000},
			},
		},
		"/proj/b": {
			Packages: []UVPackageRef{
				{Name: "numpy", Version: "2.0.0", SizeBytes: 1500},
			},
		},
	}

	total := EstimateUVSyncBytes(manifests)
	// Different versions → both counted
	if total != 2500 {
		t.Errorf("total = %d, want 2500", total)
	}
}

func TestEstimateUVSyncBytes_NilManifests(t *testing.T) {
	total := EstimateUVSyncBytes(nil)
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
}

func TestEstimateUVSyncBytes_PrefersInstalledBytes(t *testing.T) {
	manifests := map[string]*UVManifestRef{
		"/proj/a": {
			Packages: []UVPackageRef{
				{Name: "vllm", Version: "0.6.0", SizeBytes: 700, InstalledBytes: 3200},
			},
		},
		"/proj/b": {
			Packages: []UVPackageRef{
				{Name: "vllm", Version: "0.6.0", SizeBytes: 800, InstalledBytes: 3000},
			},
		},
	}

	total := EstimateUVSyncBytes(manifests)
	if total != 3200 {
		t.Errorf("total = %d, want 3200", total)
	}
}

// TestFetchUVManifests_EmptyCachedManifestTreatedAsMissing verifies that an
// empty (zero-package) cached manifest is treated as missing rather than
// returning an apparent uv-sync size of 0 — the bug that previously poisoned
// disk estimates after a single bad agent upload.
func TestFetchUVManifests_EmptyCachedManifestTreatedAsMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte("lock\n"), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	hashes := LockfileHash([]string{dir})
	hash := hashes[dir]
	hashHex := strings.TrimPrefix(hash, "sha256:")
	cachePath := filepath.Join(home, ".cache", "weft", "uv-manifests", hashHex, "linux-amd64.json")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	empty := UVManifestRef{
		LockfileHash: hash,
		Platform:     "linux-amd64",
		CollectedAt:  time.Now().UTC(),
		Packages:     nil,
	}
	data, _ := json.Marshal(empty)
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	manifests := FetchUVManifests(nil, hashes, "linux-amd64")
	if len(manifests) != 0 {
		t.Fatalf("expected empty manifest to be treated as missing, got %d", len(manifests))
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("expected empty cache file to be removed, got err=%v", err)
	}
}

func TestEstimateUVSyncBytes_EmptyManifests(t *testing.T) {
	manifests := map[string]*UVManifestRef{
		"/proj/a": nil,
	}
	total := EstimateUVSyncBytes(manifests)
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
}
