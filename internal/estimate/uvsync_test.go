package estimate

import (
	"os"
	"path/filepath"
	"testing"
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

func TestEstimateUVSyncBytes_EmptyManifests(t *testing.T) {
	manifests := map[string]*UVManifestRef{
		"/proj/a": nil,
	}
	total := EstimateUVSyncBytes(manifests)
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
}
