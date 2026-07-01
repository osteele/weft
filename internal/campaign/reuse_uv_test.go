package campaign

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

func writeReuseProject(t *testing.T, deps string) string {
	t.Helper()
	localDir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir local dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "pyproject.toml"), []byte("[project]\ndependencies = ["+deps+"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "uv.lock"), []byte("lock\n"), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}
	return localDir
}

func TestMatchGroupToInstance_AccountsForCachedUVSyncDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	localDir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir local dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "uv.lock"), []byte("lock\n"), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	writeCachedUVManifest(t, home, localDir, &estimate.UVManifestRef{
		LockfileHash: estimate.LockfileHash([]string{localDir})[localDir],
		Platform:     "linux-amd64",
		CollectedAt:  time.Now().UTC(),
		Packages: []estimate.UVPackageRef{
			{Name: "torch", Version: "1.0.0", InstalledBytes: 12_000_000_000},
		},
	})

	group := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 1, WorkingDir: localDir},
		},
	}
	cap := InstanceCapacity{
		Instance:   &db.Launch{DiskGB: 100, GPUMemGB: 24},
		DiskFreeGB: 10,
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance unexpectedly accepted instance with insufficient disk for uv sync")
	}
	if reason != "disk insufficient: need=12GB free=10GB" {
		t.Fatalf("reason = %q, want disk insufficiency from uv sync", reason)
	}
}

func TestMatchGroupToInstance_RejectsGroupRequiringVastCapAdd(t *testing.T) {
	group := InstanceGroup{
		VastCapAdd: []string{"SYS_ADMIN"},
		Jobs:       []*db.Job{{ID: 1}},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{DiskGB: 100, GPUMemGB: 24},
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance unexpectedly accepted group requiring vast-cap-add")
	}
	if reason != "requires fresh launch with vast-cap-add" {
		t.Fatalf("reason = %q, want capability guard", reason)
	}
}

func TestMatchGroupToInstance_RejectsCUDASetupOnCUDAOnlyImage(t *testing.T) {
	localDir := writeReuseProject(t, `"torch"`)
	group := InstanceGroup{
		Jobs: []*db.Job{{ID: 1, WorkingDir: localDir}},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			DiskGB:      100,
			GPUMemGB:    24,
			DockerImage: "nvidia/cuda:12.8.1-devel-ubuntu22.04",
		},
		DiskFreeGB: CUDAOverheadGB - 1,
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance unexpectedly accepted CUDA setup without enough free disk")
	}
	if !strings.Contains(reason, "need=18GB free=17GB") || !strings.Contains(reason, "CUDA packages not provided by image nvidia/cuda:12.8.1-devel-ubuntu22.04") {
		t.Fatalf("reason = %q, want CUDA setup disk rejection", reason)
	}

	cap.DiskFreeGB = CUDAOverheadGB
	ok, reason = MatchGroupToInstance(group, cap)
	if !ok {
		t.Fatalf("MatchGroupToInstance rejected sufficient CUDA setup disk: %s", reason)
	}
}

func TestMatchGroupToInstance_PyTorchImageUsesReducedSetupWhenShortcutLikely(t *testing.T) {
	localDir := writeReuseProject(t, `"torch"`)
	group := InstanceGroup{
		Jobs: []*db.Job{{ID: 1, WorkingDir: localDir}},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			DiskGB:      100,
			GPUMemGB:    24,
			DockerImage: "pytorch/pytorch:2.7.0-cuda12.8-cudnn9-runtime",
		},
		DiskFreeGB: CUDAOverheadWithPyTorchImageGB,
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if !ok {
		t.Fatalf("MatchGroupToInstance rejected PyTorch image reduced setup: %s", reason)
	}
}

func TestMatchGroupToInstance_PyTorchImageFallsBackWhenShortcutMismatch(t *testing.T) {
	localDir := writeReuseProject(t, `"torch"`)
	if err := os.WriteFile(filepath.Join(localDir, ".python-version"), []byte("3.13\n"), 0o644); err != nil {
		t.Fatalf("write .python-version: %v", err)
	}
	group := InstanceGroup{
		Jobs: []*db.Job{{ID: 1, WorkingDir: localDir}},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			DiskGB:      100,
			GPUMemGB:    24,
			DockerImage: "pytorch/pytorch:2.7.0-cuda12.8-cudnn9-runtime",
		},
		DiskFreeGB: CUDAOverheadWithPyTorchImageGB,
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance unexpectedly accepted PyTorch image when shortcut is not compatible")
	}
	if !strings.Contains(reason, "need=18GB free=6GB") {
		t.Fatalf("reason = %q, want full CUDA setup need", reason)
	}
}

func TestMatchGroupToInstance_RejectsExplicitRuntimeDisk(t *testing.T) {
	runtimeDisk := 24
	group := InstanceGroup{
		Jobs: []*db.Job{{
			ID: 1,
			CLIResourceOverrides: &db.CLIResourceOverrides{
				RuntimeDiskGB: &runtimeDisk,
			},
		}},
	}
	cap := InstanceCapacity{
		Instance:   &db.Launch{DiskGB: 100, GPUMemGB: 24},
		DiskFreeGB: runtimeDisk - 1,
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance unexpectedly accepted runtime disk need")
	}
	if reason != "disk insufficient: need=24GB free=23GB" {
		t.Fatalf("reason = %q, want runtime disk rejection", reason)
	}
}

func TestMatchGroupToInstance_RejectsIncrementalIsolatedScriptEnv(t *testing.T) {
	workDir := t.TempDir()
	script := `# /// script
# dependencies = ["vllm==0.19.1", "numpy"]
# ///
print("bench")
`
	if err := os.WriteFile(filepath.Join(workDir, "bench.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	job := &db.Job{
		ID:         1,
		WorkingDir: workDir,
		Command:    "bench.py",
	}
	group := InstanceGroup{Jobs: []*db.Job{job}}
	scriptEnvGB := commandDepIncrementalEnvGB(job)
	freeGB := NonCUDAOverheadGB + scriptEnvGB - 1
	cap := InstanceCapacity{
		Instance:   &db.Launch{DiskGB: 100, GPUMemGB: 24},
		DiskFreeGB: freeGB,
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance unexpectedly accepted incremental isolated env need")
	}
	want := fmt.Sprintf("need=%dGB free=%dGB", NonCUDAOverheadGB+scriptEnvGB, freeGB)
	if !strings.Contains(reason, want) || !strings.Contains(reason, "isolated script env estimate") {
		t.Fatalf("reason = %q, want %q with isolated env detail", reason, want)
	}
}

func TestMatchGroupToInstance_RejectsExplicitDiskFloorAboveInstance(t *testing.T) {
	diskFloor := 120
	group := InstanceGroup{
		Jobs: []*db.Job{{
			ID: 1,
			CLIResourceOverrides: &db.CLIResourceOverrides{
				DiskGB: &diskFloor,
			},
		}},
	}
	cap := InstanceCapacity{
		Instance:   &db.Launch{DiskGB: 100, GPUMemGB: 24},
		DiskFreeGB: 100,
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance unexpectedly accepted instance below disk floor")
	}
	if reason != "disk insufficient: disk floor=120GB instance=100GB" {
		t.Fatalf("reason = %q, want disk floor rejection", reason)
	}
}
