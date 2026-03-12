package campaign

import (
	"bufio"
	"database/sql"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/r2"
)

// BaseOverheadGB covers Docker image (~15GB) and working space (~2GB).
// Python/CUDA overhead is added separately via HasCUDAPackages.
const BaseOverheadGB = 17

// CUDAOverheadGB is the additional overhead when CUDA packages (torch, etc.)
// are detected in pyproject.toml. Covers venv (~8GB) + uv cache (~8GB) for
// Linux CUDA wheels, which are ~10x larger than macOS wheels.
const CUDAOverheadGB = 18

// NonCUDAOverheadGB is the Python overhead when no CUDA packages are detected.
const NonCUDAOverheadGB = 3

// HFCacheMultiplier accounts for HuggingFace cache structure overhead.
// The HF cache stores blobs + snapshots + refs, using ~1.5x the raw model size
// reported by the API's usedStorage field.
const HFCacheMultiplier = 1.5

// DefaultMinDiskGB is the minimum disk size for any cloud instance.
const DefaultMinDiskGB = 50

// cudaPackages are Python packages with large Linux CUDA wheels.
var cudaPackages = []string{
	"torch", "torchvision", "torchaudio",
	"nvidia-cublas", "nvidia-cuda-cupti", "nvidia-cuda-nvrtc",
	"nvidia-cuda-runtime", "nvidia-cudnn", "nvidia-cufft",
	"nvidia-curand", "nvidia-cusolver", "nvidia-cusparse",
	"nvidia-nccl", "nvidia-nvjitlink", "nvidia-nvtx",
	"jax", "jaxlib", "tensorflow", "vllm",
}

// EstimateGroupDisk computes the required disk space in GB for an instance group
// based on the deduplicated HF input footprint, deduplicated uv sync footprint,
// and fixed workspace/runtime overhead. Returns at least DefaultMinDiskGB.
func EstimateGroupDisk(group InstanceGroup, localDB *sql.DB, r2Client *r2.Client) int {
	var hfBytes int64
	totalBytes, err := dataloc.ResolveInputSizes(group.AllInputs(), localDB)
	if err != nil {
		log.Printf("warning: resolving input sizes: %v; continuing without HF input sizes", err)
	} else {
		hfBytes = totalBytes
	}

	overhead := BaseOverheadGB
	if hasCUDAPackages(group.SourceDirs()) {
		overhead += CUDAOverheadGB
	} else {
		overhead += NonCUDAOverheadGB
	}

	uvBytes := estimateGroupUVBytes(group.SourceDirs(), r2Client)

	diskGB := int(math.Ceil(float64(hfBytes) / 1e9 * HFCacheMultiplier))
	diskGB += int(math.Ceil(float64(uvBytes) / 1e9))
	diskGB += overhead
	if diskGB < DefaultMinDiskGB {
		return DefaultMinDiskGB
	}
	return diskGB
}

func estimateGroupUVBytes(sourceDirs []string, r2Client *r2.Client) int64 {
	lockfileHashes := estimate.LockfileHash(sourceDirs)
	if len(lockfileHashes) == 0 {
		return 0
	}
	manifests := estimate.FetchUVManifests(r2Client, lockfileHashes, "linux-amd64")
	if len(manifests) == 0 {
		return 0
	}
	return estimate.EstimateUVSyncBytes(manifests)
}

// hasCUDAPackages checks whether any pyproject.toml in the given directories
// references large CUDA packages (torch, nvidia-*, jax, tensorflow).
// Uses substring matching, which biases toward over-estimation (safe).
func hasCUDAPackages(dirs []string) bool {
	for _, dir := range dirs {
		if hasCUDAInPyproject(filepath.Join(dir, "pyproject.toml")) {
			return true
		}
	}
	return false
}

// hasCUDAInPyproject scans a pyproject.toml for references to CUDA packages.
// Uses simple line scanning rather than full TOML parsing.
func hasCUDAInPyproject(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.ToLower(scanner.Text())
		for _, pkg := range cudaPackages {
			if strings.Contains(line, pkg) {
				return true
			}
		}
	}
	return false
}
