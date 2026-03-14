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
	"github.com/osteele/weft/internal/db"
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

// EmpiricalDiskSafetyMultiplier inflates observed peak disk to leave room for
// run-to-run variation when we have historical measurements for similar jobs.
const EmpiricalDiskSafetyMultiplier = 1.15

// EmpiricalDiskSafetyGB is an additive headroom buffer (in GB) on top of the
// multiplicative empirical safety margin.
const EmpiricalDiskSafetyGB = 5

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
// and fixed project/runtime overhead. Returns at least DefaultMinDiskGB.
func EstimateGroupDisk(group InstanceGroup, localDB *sql.DB, r2Client *r2.Client) int {
	if diskGB, ok := estimateGroupDiskFromHistory(group, localDB); ok {
		return diskGB
	}

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

func estimateGroupDiskFromHistory(group InstanceGroup, localDB *sql.DB) (int, bool) {
	if localDB == nil || len(group.Jobs) == 0 {
		return 0, false
	}

	seenSigs := make(map[string]bool)
	var groupPeakBytes int64
	for _, job := range group.Jobs {
		if job == nil {
			return 0, false
		}
		sig, ok := diskHistorySignature(job)
		if !ok {
			return 0, false
		}
		if seenSigs[sig] {
			continue
		}
		seenSigs[sig] = true
		peakBytes, found, err := estimateHistoricalPeakDiskBytes(localDB, job.Project, sig)
		if err != nil {
			log.Printf("warning: estimating historical disk for job %d: %v; falling back to input sizes", job.ID, err)
			return 0, false
		}
		if !found {
			return 0, false
		}
		groupPeakBytes = max(groupPeakBytes, peakBytes)
	}
	if groupPeakBytes <= 0 {
		return 0, false
	}

	return empiricalRequiredDiskGB(groupPeakBytes), true
}

func estimateHistoricalPeakDiskBytes(localDB *sql.DB, project, targetSig string) (int64, bool, error) {
	rows, err := localDB.Query(
		`SELECT j.command,
		        COALESCE(jpt.disk_used_bytes, 0),
		        COALESCE(ts.peak_used_bytes, 0)
		   FROM jobs j
		   LEFT JOIN job_phase_timings jpt ON jpt.job_id = j.id
		   LEFT JOIN (
		     SELECT job_id,
		            MAX(
		              CASE
		                WHEN disk_total_bytes > 0 AND disk_free_bytes >= 0 AND disk_total_bytes >= disk_free_bytes
		                THEN disk_total_bytes - disk_free_bytes
		                ELSE 0
		              END
		            ) AS peak_used_bytes
		       FROM job_timeseries
		      WHERE job_id IN (SELECT id FROM jobs WHERE project = ?)
		      GROUP BY job_id
		   ) ts ON ts.job_id = j.id
		  WHERE j.project = ?`,
		project, project,
	)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()

	var peakBytes int64
	for rows.Next() {
		var command string
		var diskUsedBytes int64
		var peakUsedBytes int64
		if err := rows.Scan(&command, &diskUsedBytes, &peakUsedBytes); err != nil {
			return 0, false, err
		}
		sig, ok := commandHistorySignature(project, command)
		if !ok || sig != targetSig {
			continue
		}
		peakBytes = max(peakBytes, max(diskUsedBytes, peakUsedBytes))
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return peakBytes, peakBytes > 0, nil
}

func diskHistorySignature(job *db.Job) (string, bool) {
	if job == nil {
		return "", false
	}
	return commandHistorySignature(job.Project, job.Command)
}

func commandHistorySignature(project, command string) (string, bool) {
	if strings.TrimSpace(project) == "" {
		return "", false
	}
	_, normalized, _ := db.NormalizeCommand(command)
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return "", false
	}
	return project + "\x00" + normalized, true
}

func empiricalRequiredDiskGB(peakUsedBytes int64) int {
	requiredBytes := int64(float64(peakUsedBytes) * EmpiricalDiskSafetyMultiplier)
	requiredBytes += EmpiricalDiskSafetyGB * 1_000_000_000
	diskGB := int(math.Ceil(float64(requiredBytes) / 1e9))
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
