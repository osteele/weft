package campaign

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
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

// DiskTelemetryPlausibilityBytes is the upper bound for a single historical
// disk reading we will trust as input to the empirical estimator. Any sample
// above this is treated as anomalous (almost certainly a unit-conversion or
// statfs Bsize-vs-Frsize bug in the source telemetry) and skipped. 2 TB sits
// well above the largest rental disk we'd ever provision and well below the
// hundreds-of-TB / PB scale that legitimate bugs have produced.
const DiskTelemetryPlausibilityBytes int64 = 2 * 1000 * 1000 * 1000 * 1000

// DiskTelemetryAnomaly records a single telemetry sample that exceeded
// DiskTelemetryPlausibilityBytes and was rejected by the estimator.
// These are emitted for surfacing in placement_reasons and `weft job anomalies`
// rather than silently clamped, so the underlying bug stays visible.
type DiskTelemetryAnomaly struct {
	JobID         int64
	Project       string
	Command       string
	Source        string // "phase_timings" (disk_used_bytes) or "timeseries" (peak_used_bytes)
	ObservedBytes int64
	BoundBytes    int64
}

// recordDiskTelemetryAnomalies surfaces estimator-rejected telemetry samples
// via three channels:
//   - Structured logs (`anomalous_disk_telemetry` event) — durable, scrapable.
//   - placement_reasons on each current job whose group triggered the
//     anomaly — appears in the TUI and `weft job diagnose` for the unplaced
//     job that suffered the inflated estimate.
//   - The raw anomaly records are detected on demand by `weft job anomalies`
//     (no separate anomalies table is materialized today; the underlying
//     telemetry rows are the source of truth). The signature already carries
//     enough fields (JobID, Project, Command, Source, ObservedBytes,
//     BoundBytes) that a future extraction to a telemetry_anomalies table
//     is a query swap, not a schema migration.
//
// We deliberately do NOT silently clamp the bad sample inside the estimator —
// that would mask future bugs of the same shape. Loud surfaces only.
func recordDiskTelemetryAnomalies(localDB *sql.DB, groups []InstanceGroup, anomalies []DiskTelemetryAnomaly) {
	if len(anomalies) == 0 {
		return
	}
	seen := make(map[string]bool, len(anomalies))
	for _, a := range anomalies {
		key := fmt.Sprintf("%d|%s", a.JobID, a.Source)
		if seen[key] {
			continue
		}
		seen[key] = true
		slog.Warn("anomalous_disk_telemetry",
			"component", "disk",
			"source_job_id", a.JobID,
			"source", a.Source,
			"project", a.Project,
			"observed_bytes", a.ObservedBytes,
			"bound_bytes", a.BoundBytes,
			"note", "telemetry sample exceeds plausibility bound; estimator rejected it. Likely cause: statfs Bsize-vs-Frsize on overlay/fuse filesystems (cmd/agent/heartbeat.go, internal/runner/probes.go)")
	}
	if localDB == nil {
		return
	}
	// Build a human-readable summary of the rejected samples and prepend it
	// to placement_reasons for every current job in the affected groups.
	// We prepend rather than overwrite so the autopilot's own placement
	// failure reasons remain visible underneath; we accept that a subsequent
	// placement pass may overwrite this entry — by then the anomaly has
	// already shown up in the TUI / diagnose output, and the structured log
	// + `weft job anomalies` scan provide the durable surface.
	summary := formatDiskAnomalySummary(anomalies)
	if summary == "" {
		return
	}
	for _, group := range groups {
		for _, job := range group.Jobs {
			if job == nil {
				continue
			}
			fresh, err := db.GetJobByID(localDB, job.ID)
			if err != nil {
				slog.Warn("read placement_reasons for anomaly surface failed", "component", "disk", "job_id", job.ID, "error", err)
				continue
			}
			existing := []string{}
			if fresh != nil {
				existing = fresh.PlacementReasons
			}
			// Avoid duplicating the entry if a previous estimation pass
			// already prepended the same summary and the autopilot has
			// not yet overwritten it.
			if len(existing) > 0 && existing[0] == summary {
				continue
			}
			merged := append([]string{summary}, existing...)
			if err := db.SetJobPlacementReasons(localDB, job.ID, merged); err != nil {
				slog.Warn("write placement_reasons for anomaly surface failed", "component", "disk", "job_id", job.ID, "error", err)
			}
		}
	}
}

// formatDiskAnomalySummary renders a single-line placement_reasons entry that
// names the suspect source jobs and their reported sizes. Kept short so it
// fits in the TUI without truncation.
func formatDiskAnomalySummary(anomalies []DiskTelemetryAnomaly) string {
	if len(anomalies) == 0 {
		return ""
	}
	// Dedupe by source job ID; report the larger observed sample if both
	// the phase_timings and timeseries readings were anomalous.
	type entry struct {
		observed int64
	}
	byJob := make(map[int64]entry)
	var order []int64
	for _, a := range anomalies {
		cur, seen := byJob[a.JobID]
		if !seen || a.ObservedBytes > cur.observed {
			byJob[a.JobID] = entry{observed: a.ObservedBytes}
		}
		if !seen {
			order = append(order, a.JobID)
		}
	}
	bound := DiskTelemetryPlausibilityBytes
	var b strings.Builder
	b.WriteString("skipped anomalous historical disk reading from ")
	for i, jobID := range order {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "wj%d (%s)", jobID, humanBytes(byJob[jobID].observed))
	}
	fmt.Fprintf(&b, " > %s plausibility bound; likely statfs Bsize-vs-Frsize bug — see `weft job anomalies`", humanBytes(bound))
	return b.String()
}

// humanBytes formats a byte count using decimal (1000-based) units, which
// matches how disk sizes are reported elsewhere in weft (GB, not GiB).
func humanBytes(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n) / 1000.0
	idx := 0
	for v >= 1000 && idx < len(units)-1 {
		v /= 1000.0
		idx++
	}
	return fmt.Sprintf("%.1f%s", v, units[idx])
}

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
	"jax", "jaxlib", "tensorflow", "vllm", "sglang",
}

// EstimateGroupDisk computes the required disk space in GB for an instance group
// based on the deduplicated HF input footprint, deduplicated uv sync footprint,
// explicit runtime headroom, and fixed project overhead. Returns at least
// DefaultMinDiskGB.
//
// The returned anomalies slice carries any historical telemetry samples that
// exceeded DiskTelemetryPlausibilityBytes and were rejected. Callers should
// surface these (via placement_reasons / `weft job anomalies`) rather than
// silently swallow them — the underlying bug needs to stay visible.
func EstimateGroupDisk(group InstanceGroup, localDB *sql.DB, r2Client *r2.Client) (int, []DiskTelemetryAnomaly) {
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
	historyGB, anomalies, ok := estimateGroupDiskFromHistory(group, localDB)
	if ok && historyGB > diskGB {
		diskGB = historyGB
	}

	if diskGB < DefaultMinDiskGB {
		diskGB = DefaultMinDiskGB
	}
	if floor := groupDiskFloorGB(group); floor > diskGB {
		diskGB = floor
	}
	return diskGB, anomalies
}

func estimateGroupDiskFromHistory(group InstanceGroup, localDB *sql.DB) (int, []DiskTelemetryAnomaly, bool) {
	if localDB == nil || len(group.Jobs) == 0 {
		return 0, nil, false
	}

	seenSigs := make(map[string]bool)
	var groupPeakBytes int64
	var allAnomalies []DiskTelemetryAnomaly
	allFound := true
	for _, job := range group.Jobs {
		if job == nil {
			return 0, allAnomalies, false
		}
		sig, ok := diskHistorySignature(job)
		if !ok {
			// Without a usable signature we can't usefully look up
			// history, but we still want any already-collected
			// anomalies surfaced to the caller.
			return 0, allAnomalies, false
		}
		if seenSigs[sig] {
			continue
		}
		seenSigs[sig] = true
		peakBytes, anomalies, found, err := estimateHistoricalPeakDiskBytes(localDB, job.Project, sig)
		// Always merge anomalies, even when the row is unusable for
		// estimation — bad telemetry is still a thing to surface.
		allAnomalies = append(allAnomalies, anomalies...)
		if err != nil {
			slog.Warn("estimating historical disk failed, falling back to input sizes", "component", "disk", "job_id", job.ID, "error", err)
			return 0, allAnomalies, false
		}
		if !found {
			allFound = false
			continue
		}
		groupPeakBytes = max(groupPeakBytes, peakBytes)
	}
	if !allFound || groupPeakBytes <= 0 {
		return 0, allAnomalies, false
	}

	return empiricalRequiredDiskGB(groupPeakBytes), allAnomalies, true
}

func estimateHistoricalPeakDiskBytes(localDB *sql.DB, project, targetSig string) (int64, []DiskTelemetryAnomaly, bool, error) {
	rows, err := localDB.Query(
		`SELECT j.id,
		        j.command,
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
		return 0, nil, false, err
	}
	defer rows.Close()

	var peakBytes int64
	var anomalies []DiskTelemetryAnomaly
	for rows.Next() {
		var jobID int64
		var command string
		var diskUsedBytes int64
		var peakUsedBytes int64
		if err := rows.Scan(&jobID, &command, &diskUsedBytes, &peakUsedBytes); err != nil {
			return 0, anomalies, false, err
		}
		sig, ok := commandHistorySignature(project, command)
		if !ok || sig != targetSig {
			continue
		}
		// Reject samples above the plausibility bound rather than feeding
		// them into the estimator. We deliberately do not silently clamp:
		// the source telemetry is almost certainly the product of a bug
		// (statfs Bsize-vs-Frsize on overlay filesystems is the known
		// case), and silent clamping would mask future occurrences. The
		// caller is expected to surface the returned anomalies.
		if diskUsedBytes > DiskTelemetryPlausibilityBytes {
			anomalies = append(anomalies, DiskTelemetryAnomaly{
				JobID:         jobID,
				Project:       project,
				Command:       command,
				Source:        "phase_timings",
				ObservedBytes: diskUsedBytes,
				BoundBytes:    DiskTelemetryPlausibilityBytes,
			})
			diskUsedBytes = 0
		}
		if peakUsedBytes > DiskTelemetryPlausibilityBytes {
			anomalies = append(anomalies, DiskTelemetryAnomaly{
				JobID:         jobID,
				Project:       project,
				Command:       command,
				Source:        "timeseries",
				ObservedBytes: peakUsedBytes,
				BoundBytes:    DiskTelemetryPlausibilityBytes,
			})
			peakUsedBytes = 0
		}
		peakBytes = max(peakBytes, max(diskUsedBytes, peakUsedBytes))
	}
	if err := rows.Err(); err != nil {
		return 0, anomalies, false, err
	}
	return peakBytes, anomalies, peakBytes > 0, nil
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
