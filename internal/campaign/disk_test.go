package campaign

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

func TestEstimateGroupDisk_NoInputs(t *testing.T) {
	group := InstanceGroup{
		Jobs: []*db.Job{{ID: 1, Command: "echo test"}},
	}
	disk, _ := EstimateGroupDisk(group, nil, nil)
	if disk != DefaultMinDiskGB {
		t.Errorf("got %d, want %d", disk, DefaultMinDiskGB)
	}
}

func TestEstimateGroupDisk_MinFloor(t *testing.T) {
	// Even with small inputs, should return at least DefaultMinDiskGB
	group := InstanceGroup{
		Jobs: []*db.Job{{ID: 1, Inputs: []string{"~/data/file"}}},
	}
	disk, _ := EstimateGroupDisk(group, nil, nil)
	if disk < DefaultMinDiskGB {
		t.Errorf("got %d, want >= %d", disk, DefaultMinDiskGB)
	}
}

func TestCommandDepHeadroomGB_UVRunWithDedup(t *testing.T) {
	// FR4: heavy frameworks named via `uv run --with` are not in uv.lock, so
	// they must contribute explicit headroom, deduplicated across jobs.
	group := InstanceGroup{Jobs: []*db.Job{
		{ID: 1, Command: "uv run --with vllm --with torch bench.py"},
		{ID: 2, Command: "uv run --with torch other.py"},
	}}
	got := commandDepHeadroomGB(group)
	want := heavyDepInstalledGB["vllm"] + heavyDepInstalledGB["torch"]
	if got != want {
		t.Fatalf("commandDepHeadroomGB = %d, want %d (vllm+torch, torch deduped)", got, want)
	}
}

func TestCommandDepHeadroomGB_FromPEP723Script(t *testing.T) {
	// FR4: the reported case — vLLM declared in a PEP-723 block, not pyproject.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bench.py"), []byte(`# /// script
# dependencies = ["vllm>=0.5", "numpy"]
# ///
print("bench")
`), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	group := InstanceGroup{Jobs: []*db.Job{{ID: 1, Command: "uv run bench.py", WorkingDir: dir}}}
	if got := commandDepHeadroomGB(group); got != heavyDepInstalledGB["vllm"] {
		t.Fatalf("commandDepHeadroomGB = %d, want %d", got, heavyDepInstalledGB["vllm"])
	}
}

func TestEstimateGroupDisk_CommandHeavyDepsExceedFloor(t *testing.T) {
	// vllm+sglang+torch headroom plus CUDA overhead must push above the 50GB
	// floor, where previously a tiny model input left the env undersized.
	group := InstanceGroup{Jobs: []*db.Job{
		{ID: 1, Command: "uv run --with vllm --with sglang --with torch bench.py"},
	}}
	disk, _ := EstimateGroupDisk(group, nil, nil)
	if disk <= DefaultMinDiskGB {
		t.Fatalf("disk = %d, want > %d for a vllm+sglang+torch env", disk, DefaultMinDiskGB)
	}
}

func TestEstimateRuntimeDiskGB_DoesNotInferFromCommand(t *testing.T) {
	job := &db.Job{ID: 1, Command: "uv sync --project scripts/vllm-profiling && python bench.py"}
	if disk := EstimateRuntimeDiskGB(job); disk != 0 {
		t.Fatalf("runtime disk = %d, want 0 without explicit override", disk)
	}
}

func TestEstimateRuntimeDiskGB_UserOverride(t *testing.T) {
	job := &db.Job{
		ID:      1,
		Command: "uv sync --project scripts/vllm-profiling",
		Metadata: &db.JobMetadata{
			Disk: &db.JobDiskMetadata{RuntimeDiskGB: 42},
		},
	}
	if disk := EstimateRuntimeDiskGB(job); disk != 42 {
		t.Fatalf("runtime disk = %d, want override 42", disk)
	}
}

func TestEstimateRuntimeDiskGB_CLIOverrideWinsOverScript(t *testing.T) {
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# runtime-disk = 12
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	runtimeDisk := 48
	job := &db.Job{
		ID:         1,
		WorkingDir: workDir,
		Command:    "python train.py",
		CLIResourceOverrides: &db.CLIResourceOverrides{
			RuntimeDiskGB: &runtimeDisk,
		},
	}
	if disk := EstimateRuntimeDiskGB(job); disk != 48 {
		t.Fatalf("runtime disk = %d, want CLI override 48", disk)
	}
}

func TestEstimateGroupDisk_HonorsDiskFloor(t *testing.T) {
	group := InstanceGroup{
		Jobs: []*db.Job{{
			ID:      1,
			Command: "echo test",
			Metadata: &db.JobMetadata{
				Disk: &db.JobDiskMetadata{DiskGB: 96},
			},
		}},
	}
	if disk, _ := EstimateGroupDisk(group, nil, nil); disk != 96 {
		t.Fatalf("disk = %d, want explicit floor 96", disk)
	}
}

func TestEstimateGroupDisk_RereadsScriptDiskFloor(t *testing.T) {
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# disk = 96
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	group := InstanceGroup{
		Jobs: []*db.Job{{
			ID:         1,
			WorkingDir: workDir,
			Command:    "python train.py",
		}},
	}
	if disk, _ := EstimateGroupDisk(group, nil, nil); disk != 96 {
		t.Fatalf("disk = %d, want current script floor 96", disk)
	}
}

func TestEstimateGroupDisk_CLIDiskFloorWinsOverScript(t *testing.T) {
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# disk = 96
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	diskFloor := 200
	group := InstanceGroup{
		Jobs: []*db.Job{{
			ID:         1,
			WorkingDir: workDir,
			Command:    "python train.py",
			CLIResourceOverrides: &db.CLIResourceOverrides{
				DiskGB: &diskFloor,
			},
		}},
	}
	if disk, _ := EstimateGroupDisk(group, nil, nil); disk != 200 {
		t.Fatalf("disk = %d, want CLI floor 200", disk)
	}
}

func TestEstimateGroupDisk_UsesExplicitRuntimeDiskHeadroom(t *testing.T) {
	group := InstanceGroup{
		Jobs: []*db.Job{{
			ID:      1,
			Command: "uv sync --project scripts/vllm-profiling && python bench.py",
			Metadata: &db.JobMetadata{
				Disk: &db.JobDiskMetadata{RuntimeDiskGB: 12},
			},
		}},
	}
	if disk, _ := EstimateGroupDisk(group, nil, nil); disk != DefaultMinDiskGB {
		t.Fatalf("disk = %d, want floor %d; explicit runtime headroom should not exceed floor here", disk, DefaultMinDiskGB)
	}

	group.Jobs[0].Metadata.Disk.RuntimeDiskGB = 96
	if disk, _ := EstimateGroupDisk(group, nil, nil); disk != 105 {
		t.Fatalf("disk = %d, want 105 (base non-CUDA overhead plus explicit runtime headroom)", disk)
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

	disk, _ := EstimateGroupDisk(group, nil, nil)
	// BaseOverheadGB(6) + NonCUDAOverheadGB(3) + uv(45) = 54
	if disk != 54 {
		t.Fatalf("disk = %d, want 54", disk)
	}
}

// TestEstimateGroupDisk_FallsBackToLockfileCountWhenManifestMissing verifies
// the regression case for wj1586: a CUDA project whose uv.lock had no
// corresponding manifest in the local cache or R2 must fall back to a
// per-package estimate, not collapse to overhead-only.
func TestEstimateGroupDisk_FallsBackToLockfileCountWhenManifestMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(`[project]
dependencies = ["torch>=2.0", "transformer-lens>=2.0"]
`), 0o644); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}
	// Synthesize a large uv.lock with 1000 [[package]] blocks, modeling a
	// heavy ML stack like markov-attention's where torch + nvidia-cu* +
	// transformers + jupyterlab + spacy + wandb + gradio resolve into a
	// long dependency tree.
	const pkgCount = 1000
	var b []byte
	for i := 0; i < pkgCount; i++ {
		b = append(b, []byte("[[package]]\nname = \"p\"\nversion = \"1.0\"\n\n")...)
	}
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), b, 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	group := InstanceGroup{Jobs: []*db.Job{{ID: 1, WorkingDir: dir}}}
	disk, _ := EstimateGroupDisk(group, nil, nil)

	// Per-package CUDA fallback is 80 MB; 1000 × 80MB = 80 GB.
	// + BaseOverheadGB (6) + CUDAOverheadGB (18) = 104 GB. Must exceed
	// DefaultMinDiskGB by a wide margin to prove the fallback engaged
	// (overhead-only would have been 24 GB, raised to floor 50).
	if disk <= DefaultMinDiskGB {
		t.Fatalf("disk = %d, want > %d (lockfile fallback should escape floor)",
			disk, DefaultMinDiskGB)
	}
	if disk < 100 {
		t.Errorf("disk = %d, want >= ~100 GB for %d-package CUDA lockfile", disk, pkgCount)
	}

	// Tiny lockfile should stay at the floor (no over-provisioning).
	smallDir := t.TempDir()
	os.WriteFile(filepath.Join(smallDir, "pyproject.toml"), []byte("[project]\ndependencies = [\"requests\"]\n"), 0o644)
	os.WriteFile(filepath.Join(smallDir, "uv.lock"), []byte("[[package]]\nname=\"p\"\nversion=\"1\"\n"), 0o644)
	smallGroup := InstanceGroup{Jobs: []*db.Job{{ID: 2, WorkingDir: smallDir}}}
	smallDisk, _ := EstimateGroupDisk(smallGroup, nil, nil)
	if smallDisk != DefaultMinDiskGB {
		t.Errorf("small lockfile disk = %d, want %d (floor)", smallDisk, DefaultMinDiskGB)
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

	disk, _ := EstimateGroupDisk(llmPerfModelsTestGroup(), database, nil)
	if disk != 60 {
		t.Fatalf("disk = %d, want 60", disk)
	}
}

func TestEstimateGroupDisk_FallsBackWhenAnyJobLacksHistory(t *testing.T) {
	database := db.SetupTestDB(t)
	addHistoricalDiskRecord(t, database, "llm-performance-models", "bash scripts/collect_all.sh --skip-vllm", 47_607_521_280)
	// Second job has no history → should fall back

	disk, _ := EstimateGroupDisk(llmPerfModelsTestGroup(), database, nil)
	if disk != DefaultMinDiskGB {
		t.Fatalf("disk = %d, want %d", disk, DefaultMinDiskGB)
	}
}

// TestEstimateGroupDisk_UnresolvedInputsGetFallback verifies that when some HF
// refs resolve but one fails (e.g. a gated model, or a dataset ref
// misrouted as a model like "hf:wikitext"), the estimator uses the
// successfully resolved bytes PLUS a per-unresolved-ref fallback budget —
// not zero HF bytes, which was the bug that caused instances to be
// provisioned with too little disk and fail with disk_full.
func TestEstimateGroupDisk_UnresolvedInputsGetFallback(t *testing.T) {
	// Model "good/model" resolves (10 GB); "wikitext" 404s as a model and
	// matches the datasets endpoint (triggering ErrHFRefIsDataset).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/good/model/tree/main":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"path":"model.bin","size":10000000000}]`))
		case "/api/datasets/wikitext":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"wikitext"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(dataloc.OverrideHFURLsForTesting(server.URL, server.Client()))

	group := InstanceGroup{
		Jobs: []*db.Job{{
			ID:     1,
			Inputs: []string{"hf:good/model", "hf:wikitext"},
		}},
	}
	disk, _ := EstimateGroupDisk(group, nil, nil)

	// hfBytes = 10 GB (good/model), unresolvedFallback = 20 GB (hf:wikitext).
	// inputDiskGB = ceil((10 + 20) * 1.5) = 45
	// overhead (default image, non-CUDA) = BaseOverheadGB(6) + NonCUDAOverheadGB(3) = 9
	// total = 45 + 0 (no uv) + 9 = 54
	if disk < 54 {
		t.Fatalf("disk = %d, want >= 54 (resolved + unresolved fallback); "+
			"without the fix this would fall back to %d (overhead only, HF bytes dropped)",
			disk, DefaultMinDiskGB)
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

func Test_CUDAOverheadWithPyTorchImage(t *testing.T) {
	// pytorch/pytorch image should use reduced overhead
	if CUDAOverheadWithPyTorchImageGB >= CUDAOverheadGB {
		t.Errorf("CUDAOverheadWithPyTorchImageGB (%d) should be less than CUDAOverheadGB (%d)",
			CUDAOverheadWithPyTorchImageGB, CUDAOverheadGB)
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
