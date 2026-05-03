package campaign

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/r2"
)

// BaseOverheadGB covers the default Docker image (~4GB on disk) and working space (~2GB).
// Additional image overhead for non-default images is added by imageOverheadGB.
// Python/CUDA overhead is added separately via HasCUDAPackages.
const BaseOverheadGB = 6

// CUDAOverheadGB is the additional overhead when CUDA packages (torch, etc.)
// are detected in pyproject.toml. Covers venv (~8GB) + uv cache (~8GB) for
// Linux CUDA wheels, which are ~10x larger than macOS wheels.
const CUDAOverheadGB = 18

// CUDAOverheadWithPyTorchImageGB is the reduced overhead when using a
// pytorch/pytorch base image. Torch + CUDA runtime wheels are pre-installed
// in the image, so only non-torch wheels and uv cache are needed.
const CUDAOverheadWithPyTorchImageGB = 6

// NonCUDAOverheadGB is the Python overhead when no CUDA packages are detected.
const NonCUDAOverheadGB = 3

// HFCacheMultiplier accounts for HuggingFace cache structure overhead.
// The HF cache stores blobs + snapshots + refs, using ~1.5x the raw model size
// reported by the API's usedStorage field.
const HFCacheMultiplier = 1.5

// DefaultMinDiskGB is the minimum disk size for any cloud instance.
const DefaultMinDiskGB = 50

// UnresolvedHFFallbackGB is the conservative raw-size budget applied per HF
// input ref that could not be resolved (e.g. a gated model the HF token can't
// reach, or a dataset ref misrouted as a model). Sized to cover a mid-range
// LLM; over-provisioning disk is cheap compared to a disk_full failure.
const UnresolvedHFFallbackGB = 20

// EmpiricalDiskSafetyMultiplier inflates observed peak disk to leave room for
// run-to-run variation when we have historical measurements for similar jobs.
const EmpiricalDiskSafetyMultiplier = 1.15

// EmpiricalDiskSafetyGB is an additive headroom buffer (in GB) on top of the
// multiplicative empirical safety margin.
const EmpiricalDiskSafetyGB = 5

// imageOverheadGB returns additional disk overhead in GB for non-default Docker images.
// The default nvidia/cuda runtime image is ~4 GB on disk (accounted for in BaseOverheadGB).
// Larger images like pytorch/pytorch add extra overhead.
func imageOverheadGB(image string) int {
	if image == "" {
		return 0 // default image, already in BaseOverheadGB
	}
	if isPyTorchImage(image) {
		return 6 // pytorch runtime ~10 GB on disk vs ~4 GB default
	}
	// Unknown image — add a moderate buffer
	return 3
}

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
// explicit runtime headroom, and fixed project overhead. Returns at least
// DefaultMinDiskGB.
func EstimateGroupDisk(group InstanceGroup, localDB *sql.DB, r2Client *r2.Client) int {
	// Compute input-based estimate (always, as a floor)
	allInputs := group.AllInputs()
	if localDB != nil {
		observed := lookupObservedInputs(group, localDB)
		allInputs = mergeStringSlices(allInputs, observed)
	}

	hfBytes, unresolved, err := dataloc.ResolveInputSizes(allInputs, localDB)
	if err != nil {
		slog.Warn("some input sizes could not be resolved; applying fallback for those refs",
			"component", "disk", "unresolved", unresolved, "error", err)
	}
	unresolvedFallbackBytes := int64(len(unresolved)) * UnresolvedHFFallbackGB * 1_000_000_000

	overhead := BaseOverheadGB + imageOverheadGB(group.Image)
	if hasCUDAPackages(group.SourceDirs()) {
		if isPyTorchImage(group.Image) {
			overhead += CUDAOverheadWithPyTorchImageGB
		} else {
			overhead += CUDAOverheadGB
		}
	} else {
		overhead += NonCUDAOverheadGB
	}

	uvBytes := estimateGroupUVBytes(group.SourceDirs(), r2Client)

	inputDiskGB := int(math.Ceil(float64(hfBytes+unresolvedFallbackBytes) / 1e9 * HFCacheMultiplier))
	inputDiskGB += int(math.Ceil(float64(uvBytes) / 1e9))
	inputDiskGB += overhead
	inputDiskGB += groupRuntimeDiskGB(group)

	// Use the larger of history-based and input-based estimates.
	// History may underestimate if prior runs failed before completing.
	diskGB := inputDiskGB
	if historyGB, ok := estimateGroupDiskFromHistory(group, localDB); ok && historyGB > diskGB {
		diskGB = historyGB
	}

	if diskGB < DefaultMinDiskGB {
		diskGB = DefaultMinDiskGB
	}
	if floor := groupDiskFloorGB(group); floor > diskGB {
		diskGB = floor
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
			slog.Warn("estimating historical disk failed, falling back to input sizes", "component", "disk", "job_id", job.ID, "error", err)
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

// Per-package installed-size estimates used when no uv manifest is available.
// CUDA projects pin significantly larger wheels (torch + nvidia-* runtime).
const (
	fallbackPerPackageCUDABytes    = 80_000_000
	fallbackPerPackageNonCUDABytes = 25_000_000
)

func estimateGroupUVBytes(sourceDirs []string, r2Client *r2.Client) int64 {
	lockfileHashes := estimate.LockfileHash(sourceDirs)
	if len(lockfileHashes) == 0 {
		return 0
	}
	manifests := estimate.FetchUVManifests(r2Client, lockfileHashes, "linux-amd64")
	var total int64
	if len(manifests) > 0 {
		total = estimate.EstimateUVSyncBytes(manifests)
	}
	// Without this fallback, a missing or empty manifest collapses the
	// estimate to overhead-only and undersizes the rental.
	for dir := range lockfileHashes {
		if _, hasManifest := manifests[dir]; hasManifest {
			continue
		}
		count := estimate.CountLockfilePackages(filepath.Join(dir, "uv.lock"))
		if count == 0 {
			continue
		}
		perPkg := int64(fallbackPerPackageNonCUDABytes)
		if hasCUDAInPyproject(filepath.Join(dir, "pyproject.toml")) {
			perPkg = fallbackPerPackageCUDABytes
		}
		total += int64(count) * perPkg
	}
	return total
}

// lookupObservedInputs queries the DB for observed_inputs from prior runs of
// jobs with matching command signatures. This creates a feedback loop: if a
// previous run failed with disk-full and undeclared HF models were detected,
// future runs of the same command automatically account for those models.
func lookupObservedInputs(group InstanceGroup, localDB *sql.DB) []string {
	if localDB == nil {
		return nil
	}
	seen := make(map[string]bool)
	var result []string
	// Collect unique command signatures to query
	sigSet := make(map[string]string) // sig -> project
	for _, job := range group.Jobs {
		sig, ok := diskHistorySignature(job)
		if !ok {
			continue
		}
		sigSet[sig] = job.Project
	}
	for _, project := range sigSet {
		rows, err := localDB.Query(
			`SELECT command, observed_inputs FROM job_status WHERE project = ? AND observed_inputs IS NOT NULL AND observed_inputs != ''`,
			project,
		)
		if err != nil {
			continue
		}
		for rows.Next() {
			var command, obsJSON string
			if rows.Scan(&command, &obsJSON) != nil {
				continue
			}
			rowSig, ok := commandHistorySignature(project, command)
			if !ok {
				continue
			}
			if _, matches := sigSet[rowSig]; !matches {
				continue
			}
			var obs []string
			if json.Unmarshal([]byte(obsJSON), &obs) != nil {
				continue
			}
			for _, input := range obs {
				if !seen[input] {
					seen[input] = true
					result = append(result, input)
				}
			}
		}
		rows.Close()
	}
	return result
}

func mergeStringSlices(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	merged := append([]string(nil), a...)
	for _, s := range b {
		if !seen[s] {
			merged = append(merged, s)
		}
	}
	return merged
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
