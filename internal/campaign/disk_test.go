package campaign

import (
	"database/sql"
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
	// BaseOverheadGB(6) + NonCUDAOverheadGB(3) + uv(45) = 54
	if disk != 54 {
		t.Fatalf("disk = %d, want 54", disk)
	}
}

// addHistoricalDiskRecord records a completed job with disk usage for testing empirical disk estimation.
func addHistoricalDiskRecord(t *testing.T, database *sql.DB, project, cmd string, diskBytes int64) {
	t.Helper()
	jobID, err := db.RecordQueued(database, "", "/tmp/project", cmd, "hist")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobProject(database, jobID, project); err != nil {
		t.Fatalf("set project: %v", err)
	}
	if err := db.UpsertJobPhaseTimings(database, &db.JobPhaseTimings{
		JobID:         jobID,
		DiskUsedBytes: int64Ptr(diskBytes),
	}); err != nil {
		t.Fatalf("upsert phase timings: %v", err)
	}
}

// llmPerfModelsTestGroup returns a two-job group for the llm-performance-models project.
func llmPerfModelsTestGroup() InstanceGroup {
	return InstanceGroup{
		Jobs: []*db.Job{
			{ID: 222, Project: "llm-performance-models", Command: "bash scripts/collect_all.sh --skip-vllm"},
			{ID: 223, Project: "llm-performance-models", Command: "uv run python scripts/calibrate_power.py --duration 10 --sweep"},
		},
	}
}

func TestEstimateGroupDisk_UsesEmpiricalHistoryWhenAllJobsMatch(t *testing.T) {
	database := db.SetupTestDB(t)
	addHistoricalDiskRecord(t, database, "llm-performance-models", "bash scripts/collect_all.sh --skip-vllm", 47_607_521_280)
	addHistoricalDiskRecord(t, database, "llm-performance-models", "uv run python scripts/calibrate_power.py --duration 10 --sweep", 47_607_513_088)

	disk := EstimateGroupDisk(llmPerfModelsTestGroup(), database, nil)
	if disk != 60 {
		t.Fatalf("disk = %d, want 60", disk)
	}
}

func TestEstimateGroupDisk_FallsBackWhenAnyJobLacksHistory(t *testing.T) {
	database := db.SetupTestDB(t)
	addHistoricalDiskRecord(t, database, "llm-performance-models", "bash scripts/collect_all.sh --skip-vllm", 47_607_521_280)
	// Second job has no history → should fall back

	disk := EstimateGroupDisk(llmPerfModelsTestGroup(), database, nil)
	if disk != DefaultMinDiskGB {
		t.Fatalf("disk = %d, want %d", disk, DefaultMinDiskGB)
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

func int64Ptr(v int64) *int64 {
	return &v
}
