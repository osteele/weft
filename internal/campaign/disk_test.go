package campaign

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestEstimateGroupDisk_NoInputs(t *testing.T) {
	group := InstanceGroup{
		Jobs: []*db.Job{{ID: 1, Command: "echo test"}},
	}
	disk := EstimateGroupDisk(group, nil)
	if disk != DefaultMinDiskGB {
		t.Errorf("got %d, want %d", disk, DefaultMinDiskGB)
	}
}

func TestEstimateGroupDisk_MinFloor(t *testing.T) {
	// Even with small inputs, should return at least DefaultMinDiskGB
	group := InstanceGroup{
		Jobs: []*db.Job{{ID: 1, Inputs: []string{"~/data/file"}}},
	}
	disk := EstimateGroupDisk(group, nil)
	if disk < DefaultMinDiskGB {
		t.Errorf("got %d, want >= %d", disk, DefaultMinDiskGB)
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
