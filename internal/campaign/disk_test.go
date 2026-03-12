package campaign

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

func TestEstimateGroupDisk_NoInputs(t *testing.T) {
	group := InstanceGroup{
		Jobs: []*db.Job{{ID: 1, Command: "echo test"}},
	}
	disk := EstimateGroupDisk(group, nil, nil)
	if disk != DefaultMinDiskGB {
		t.Errorf("got %d, want %d", disk, DefaultMinDiskGB)
	}
}

func TestEstimateGroupDisk_MinFloor(t *testing.T) {
	// Even with small inputs, should return at least DefaultMinDiskGB
	group := InstanceGroup{
		Jobs: []*db.Job{{ID: 1, Inputs: []string{"~/data/file"}}},
	}
	disk := EstimateGroupDisk(group, nil, nil)
	if disk < DefaultMinDiskGB {
		t.Errorf("got %d, want >= %d", disk, DefaultMinDiskGB)
	}
}

func TestEstimateGroupDisk_UsesCachedUVManifestUnion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dirA := filepath.Join(t.TempDir(), "proj-a")
	dirB := filepath.Join(t.TempDir(), "proj-b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatalf("mkdir dirA: %v", err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatalf("mkdir dirB: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "uv.lock"), []byte("proj-a-lock\n"), 0o644); err != nil {
		t.Fatalf("write uv.lock a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "uv.lock"), []byte("proj-b-lock\n"), 0o644); err != nil {
		t.Fatalf("write uv.lock b: %v", err)
	}

	writeCachedUVManifest(t, home, dirA, &estimate.UVManifestRef{
		LockfileHash: estimate.LockfileHash([]string{dirA})[dirA],
		Platform:     "linux-amd64",
		CollectedAt:  time.Now().UTC(),
		Packages: []estimate.UVPackageRef{
			{Name: "shared", Version: "1.0.0", InstalledBytes: 10_000_000_000},
			{Name: "only-a", Version: "1.0.0", InstalledBytes: 20_000_000_000},
		},
	})
	writeCachedUVManifest(t, home, dirB, &estimate.UVManifestRef{
		LockfileHash: estimate.LockfileHash([]string{dirB})[dirB],
		Platform:     "linux-amd64",
		CollectedAt:  time.Now().UTC(),
		Packages: []estimate.UVPackageRef{
			{Name: "shared", Version: "1.0.0", InstalledBytes: 9_000_000_000},
			{Name: "only-b", Version: "1.0.0", InstalledBytes: 15_000_000_000},
		},
	})

	group := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 1, WorkingDir: dirA},
			{ID: 2, WorkingDir: dirB},
		},
	}

	disk := EstimateGroupDisk(group, nil, nil)
	if disk != 65 {
		t.Fatalf("disk = %d, want 65", disk)
	}
}

func Test_hasCUDAPackages(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "torch dependency",
			content: "[project]\ndependencies = [\n  \"torch>=2.0\",\n]\n",
			want:    true,
		},
		{
			name:    "no CUDA packages",
			content: "[project]\ndependencies = [\n  \"requests\",\n  \"numpy\",\n]\n",
			want:    false,
		},
		{
			name:    "nvidia package",
			content: "[project]\ndependencies = [\n  \"nvidia-cuda-runtime\",\n]\n",
			want:    true,
		},
		{
			name:    "jax dependency",
			content: "[project]\ndependencies = [\n  \"jax[cuda12]\",\n]\n",
			want:    true,
		},
		{
			name:    "vllm dependency",
			content: "[project]\ndependencies = [\n  \"vllm>=0.6\",\n]\n",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subdir := filepath.Join(dir, tt.name)
			os.MkdirAll(subdir, 0755)
			os.WriteFile(filepath.Join(subdir, "pyproject.toml"), []byte(tt.content), 0644)

			got := hasCUDAPackages([]string{subdir})
			if got != tt.want {
				t.Errorf("hasCUDAPackages() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_hasCUDAPackages_NoPyproject(t *testing.T) {
	dir := t.TempDir()
	if hasCUDAPackages([]string{dir}) {
		t.Error("expected false when no pyproject.toml exists")
	}
}

func writeCachedUVManifest(t *testing.T, home, dir string, manifest *estimate.UVManifestRef) {
	t.Helper()

	hash := estimate.LockfileHash([]string{dir})[dir]
	hashHex := hash[len("sha256:"):]
	cachePath := filepath.Join(home, ".cache", "weft", "uv-manifests", hashHex, "linux-amd64.json")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir manifest cache: %v", err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
