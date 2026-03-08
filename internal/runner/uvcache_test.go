package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectUVManifest_NoLockfile(t *testing.T) {
	dir := t.TempDir()
	m, err := CollectUVManifest(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m != nil {
		t.Error("expected nil manifest when no uv.lock exists")
	}
}

func TestCollectUVManifest_WithLockfile(t *testing.T) {
	dir := t.TempDir()

	// Create a uv.lock file
	lockContent := []byte("some lock content\n")
	os.WriteFile(filepath.Join(dir, "uv.lock"), lockContent, 0644)

	// Create a mock uv cache with wheel files
	cacheDir := filepath.Join(dir, "uv-cache")
	wheelsDir := filepath.Join(cacheDir, "wheels-v3", "index", "abc123")
	os.MkdirAll(wheelsDir, 0755)

	// Write mock wheel files
	os.WriteFile(filepath.Join(wheelsDir, "numpy-1.26.4-cp312-cp312-linux_x86_64.whl"), make([]byte, 1000), 0644)
	os.WriteFile(filepath.Join(wheelsDir, "torch-2.2.1-cp312-cp312-linux_x86_64.whl"), make([]byte, 5000), 0644)

	// Point UV_CACHE_DIR to our mock cache
	t.Setenv("UV_CACHE_DIR", cacheDir)

	m, err := CollectUVManifest(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil manifest")
	}

	if m.LockfileHash == "" {
		t.Error("lockfile hash should not be empty")
	}
	if len(m.LockfileHash) < 10 {
		t.Errorf("lockfile hash too short: %s", m.LockfileHash)
	}
	if m.Platform == "" {
		t.Error("platform should not be empty")
	}

	if len(m.Packages) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(m.Packages))
	}

	pkgByName := make(map[string]UVPackage)
	for _, p := range m.Packages {
		pkgByName[p.Name] = p
	}

	np, ok := pkgByName["numpy"]
	if !ok {
		t.Fatal("missing numpy package")
	}
	if np.Version != "1.26.4" {
		t.Errorf("numpy version = %q, want 1.26.4", np.Version)
	}
	if np.SizeBytes != 1000 {
		t.Errorf("numpy size = %d, want 1000", np.SizeBytes)
	}

	torch, ok := pkgByName["torch"]
	if !ok {
		t.Fatal("missing torch package")
	}
	if torch.Version != "2.2.1" {
		t.Errorf("torch version = %q, want 2.2.1", torch.Version)
	}
	if torch.SizeBytes != 5000 {
		t.Errorf("torch size = %d, want 5000", torch.SizeBytes)
	}
}

func TestCollectUVManifest_DeterministicHash(t *testing.T) {
	dir := t.TempDir()
	lockContent := []byte("deterministic test content\n")
	os.WriteFile(filepath.Join(dir, "uv.lock"), lockContent, 0644)
	t.Setenv("UV_CACHE_DIR", t.TempDir()) // empty cache

	m1, _ := CollectUVManifest(dir)
	m2, _ := CollectUVManifest(dir)

	if m1.LockfileHash != m2.LockfileHash {
		t.Errorf("hash not deterministic: %s != %s", m1.LockfileHash, m2.LockfileHash)
	}
}

func TestWriteUVManifest(t *testing.T) {
	dir := t.TempDir()
	m := &UVManifest{
		LockfileHash: "sha256:abc123",
		Platform:     "linux-amd64",
		Packages: []UVPackage{
			{Name: "numpy", Version: "1.26.4", SizeBytes: 1000},
		},
	}

	if err := WriteUVManifest(dir, m); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "uv-manifest.json"))
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	var loaded UVManifest
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if loaded.LockfileHash != "sha256:abc123" {
		t.Errorf("hash = %q, want sha256:abc123", loaded.LockfileHash)
	}
	if len(loaded.Packages) != 1 {
		t.Fatalf("packages = %d, want 1", len(loaded.Packages))
	}
}

func TestNormalizePackageName(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"numpy", "numpy"},
		{"Pillow", "pillow"},
		{"my_package", "my-package"},
		{"some.pkg.name", "some-pkg-name"},
		{"Mixed_Case.Name", "mixed-case-name"},
		{"double__underscore", "double-underscore"},
	}
	for _, tt := range tests {
		got := normalizePackageName(tt.input)
		if got != tt.want {
			t.Errorf("normalizePackageName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
