// Package placement scores hosts for job placement based on resource
// constraints, data locality, and current utilization.
package placement

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/inventory"
)

// ErrNoReachableHost is returned when all eligible hosts are unreachable.
var ErrNoReachableHost = errors.New("no eligible host is reachable")

// Constraints describes hard requirements for a job placement.
type Constraints struct {
	GPUClass string   // Required GPU class (e.g., "a100"); empty = no preference
	GPUMemGB int      // Minimum GPU memory in GB; 0 = no minimum
	Inputs   []string // Asset refs the job reads (for locality scoring)
	Command  string   // For predictor-based scoring; empty = skip
	Project  string   // For predictor-based scoring; empty = skip
}

// HostMetrics holds live utilization data for a host, used for soft scoring.
// All fields are optional; -1 or 0 means "unknown/unavailable".
type HostMetrics struct {
	CPUPercent    int   // 0-100, from load average / core count
	GPUPercent    int   // 0-100, max utilization across GPUs
	RAMPercent    int   // 0-100
	QueueDepth    int   // number of queued (pending) jobs
	FreeRAMKB     int64 // available RAM in KB; 0 = unknown
	FreeGPUMemMiB int64 // available GPU memory in MiB; 0 = unknown

	// Per-GPU free memory in MiB, keyed by device index (e.g. "0", "1").
	// Used for per-device placement scoring when GPUClass constraint is set.
	GPUDeviceFreeMemMiB map[string]int64

	// Number of GPU jobs queued for this host's GPU class.
	// Used for soft penalty to spread load across hosts.
	GPUJobsQueued int
}

// JobPrediction holds predicted resource needs for a job on a specific host.
type JobPrediction struct {
	DurationS    *float64 // predicted wall-clock seconds (nil = unknown)
	PeakRSSKB    *float64 // predicted peak RSS in KB (nil = unknown)
	MaxGPUMemMiB *float64 // predicted peak GPU memory in MiB (nil = unknown)
	// Upper bounds (95% CI) for hard constraint checking
	PeakRSSKBUpper    *float64
	MaxGPUMemMiBUpper *float64
}

// JobPredictor returns predicted resource needs for a job on a given host.
// Returns nil if prediction is unavailable.
type JobPredictor func(host string) *JobPrediction

// RawPredictionField holds a point estimate with uncertainty bounds.
type RawPredictionField struct {
	Mean  float64
	Upper float64
}

// RawPrediction is a generic prediction result that can be converted to JobPrediction.
type RawPrediction struct {
	DurationS    *RawPredictionField
	PeakRSSKB    *RawPredictionField
	MaxGPUMemMiB *RawPredictionField
}

// NewJobPredictor builds a JobPredictor from a function that returns raw predictions.
// This avoids duplicating the RawPrediction → JobPrediction mapping at each call site.
func NewJobPredictor(predict func(host string) *RawPrediction) JobPredictor {
	if predict == nil {
		return nil
	}
	return func(host string) *JobPrediction {
		raw := predict(host)
		if raw == nil {
			return nil
		}
		jp := &JobPrediction{}
		if raw.DurationS != nil {
			jp.DurationS = &raw.DurationS.Mean
		}
		if raw.PeakRSSKB != nil {
			jp.PeakRSSKB = &raw.PeakRSSKB.Mean
			jp.PeakRSSKBUpper = &raw.PeakRSSKB.Upper
		}
		if raw.MaxGPUMemMiB != nil {
			jp.MaxGPUMemMiB = &raw.MaxGPUMemMiB.Mean
			jp.MaxGPUMemMiBUpper = &raw.MaxGPUMemMiB.Upper
		}
		return jp
	}
}

// Score represents the placement score for a single host.
type Score struct {
	Host              string
	Total             float64 // Higher is better
	Eligible          bool    // Passes all hard constraints
	Reasons           []string
	staticPerfPenalty float64 // penalty from cpu_factor/gpu_factor, tracked for reversal
}

// ScoreHosts evaluates all inventory hosts against the given constraints
// and returns scores sorted best-first. Ineligible hosts are included
// but marked accordingly. Does not consider live utilization.
func ScoreHosts(db *sql.DB, constraints Constraints) ([]Score, error) {
	return ScoreHostsWithMetrics(db, constraints, nil)
}

// ScoreHostsWithMetrics evaluates hosts with optional live utilization data.
// The metrics map is keyed by host name. Nil or missing entries are skipped.
func ScoreHostsWithMetrics(db *sql.DB, constraints Constraints, metrics map[string]*HostMetrics) ([]Score, error) {
	hosts, err := inventory.LoadEmbeddedHosts()
	if err != nil {
		return nil, fmt.Errorf("load inventory: %w", err)
	}
	scores := scoreAll(db, hosts, constraints, metrics)
	sortScores(scores)
	return scores, nil
}

// ScoreHostsWithPredictor evaluates hosts with optional predictor and metrics.
func ScoreHostsWithPredictor(db *sql.DB, constraints Constraints, metrics map[string]*HostMetrics, predict JobPredictor) ([]Score, error) {
	hosts, err := inventory.LoadEmbeddedHosts()
	if err != nil {
		return nil, fmt.Errorf("load inventory: %w", err)
	}
	scores := scoreAll(db, hosts, constraints, metrics)

	if predict == nil {
		sortScores(scores)
		return scores, nil
	}

	hostSpecs := make(map[string]inventory.HostSpec, len(hosts))
	for _, h := range hosts {
		hostSpecs[h.Name] = h
	}

	// Collect predictions and apply resource constraints in a single pass
	predictions := make(map[string]*JobPrediction)
	for i := range scores {
		if !scores[i].Eligible {
			continue
		}
		p := predict(scores[i].Host)
		if p == nil {
			continue
		}
		predictions[scores[i].Host] = p

		applyResourceHardConstraints(&scores[i], p, hostSpecs[scores[i].Host])
		if !scores[i].Eligible {
			continue
		}

		var m *HostMetrics
		if metrics != nil {
			m = metrics[scores[i].Host]
		}
		applyResourceSoftConstraints(&scores[i], p, m)
	}

	applyDurationScoring(scores, predictions)
	sortScores(scores)
	return scores, nil
}

// scoreAll runs scoreHost for each host. Does not sort.
func scoreAll(db *sql.DB, hosts []inventory.HostSpec, constraints Constraints, metrics map[string]*HostMetrics) []Score {
	scores := make([]Score, 0, len(hosts))
	for _, h := range hosts {
		var m *HostMetrics
		if metrics != nil {
			m = metrics[h.Name]
		}
		scores = append(scores, scoreHost(db, h, constraints, m))
	}
	return scores
}

// BestHostWithPredictor returns the best eligible host considering predictor and metrics.
func BestHostWithPredictor(db *sql.DB, constraints Constraints, metrics map[string]*HostMetrics, predict JobPredictor) (string, []string, error) {
	scores, err := ScoreHostsWithPredictor(db, constraints, metrics, predict)
	if err != nil {
		return "", nil, err
	}
	return bestFromScores(scores, constraints)
}

// BestHost returns the best eligible host, or an error if none qualify.
func BestHost(db *sql.DB, constraints Constraints) (string, []string, error) {
	return BestHostWithMetrics(db, constraints, nil)
}

// BestHostWithMetrics returns the best eligible host considering live metrics.
func BestHostWithMetrics(db *sql.DB, constraints Constraints, metrics map[string]*HostMetrics) (string, []string, error) {
	scores, err := ScoreHostsWithMetrics(db, constraints, metrics)
	if err != nil {
		return "", nil, err
	}
	return bestFromScores(scores, constraints)
}

func bestFromScores(scores []Score, constraints Constraints) (string, []string, error) {
	for _, s := range scores {
		if s.Eligible {
			return s.Host, s.Reasons, nil
		}
	}
	return "", nil, fmt.Errorf("no eligible host found for constraints: %s", describeConstraints(constraints))
}

// BestReachableHost returns the best eligible host that is also reachable via SSH.
// It scores hosts, probes eligible ones in parallel, and returns the highest-scored
// reachable host. Returns ErrNoReachableHost if no eligible host responds.
func BestReachableHost(db *sql.DB, constraints Constraints, probeTimeout time.Duration) (string, []string, error) {
	scores, err := ScoreHosts(db, constraints)
	if err != nil {
		return "", nil, err
	}

	var eligible []string
	for _, s := range scores {
		if s.Eligible {
			eligible = append(eligible, s.Host)
		}
	}
	if len(eligible) == 0 {
		return "", nil, fmt.Errorf("no eligible host found for constraints: %s", describeConstraints(constraints))
	}

	liveness := ProbeHosts(eligible, probeTimeout)

	for _, s := range scores {
		if s.Eligible && liveness[s.Host] {
			return s.Host, s.Reasons, nil
		}
	}

	return "", nil, ErrNoReachableHost
}

func scoreHost(db *sql.DB, host inventory.HostSpec, c Constraints, metrics *HostMetrics) Score {
	s := Score{Host: host.Name, Eligible: true}

	// Hard constraint: GPU class (normalized: strip spaces/punctuation, case-insensitive)
	if c.GPUClass != "" {
		norm := normalizeGPUClass(c.GPUClass)
		var matchedName string
		for _, gpu := range host.GPUs {
			if normalizeGPUClass(gpu.Class) == norm {
				matchedName = gpu.Name
				break
			}
		}
		if matchedName == "" {
			s.Eligible = false
			s.Reasons = append(s.Reasons, fmt.Sprintf("no %s GPU", c.GPUClass))
			return s
		}
		s.Total += 10
		s.Reasons = append(s.Reasons, fmt.Sprintf("has %s GPU", matchedName))
	}

	// Hard constraint: GPU memory
	if c.GPUMemGB > 0 {
		found := false
		for _, gpu := range host.GPUs {
			memGB := parseMemGB(gpu.Memory)
			if memGB >= c.GPUMemGB {
				found = true
				break
			}
		}
		if !found {
			s.Eligible = false
			s.Reasons = append(s.Reasons, fmt.Sprintf("no GPU with >=%dGB", c.GPUMemGB))
			return s
		}
		s.Reasons = append(s.Reasons, fmt.Sprintf("has GPU with >=%dGB", c.GPUMemGB))
	}

	// Soft factor: data locality + transfer cost
	if db != nil && len(c.Inputs) > 0 {
		localCount := 0
		var totalMissingBytes int64
		missingCount := 0
		for _, ref := range c.Inputs {
			asset, ok := dataloc.ParseAssetRef(ref)
			if !ok {
				continue
			}
			entries, err := dataloc.FindAssetHosts(db, asset)
			if err != nil {
				continue
			}
			isLocal := false
			for _, e := range entries {
				if e.Host == host.Name {
					isLocal = true
					localCount++
					break
				}
			}
			if !isLocal && len(entries) > 0 {
				// Use the largest known size across hosts
				var maxSize int64
				for _, e := range entries {
					if e.SizeBytes > maxSize {
						maxSize = e.SizeBytes
					}
				}
				totalMissingBytes += maxSize
				missingCount++
			}
		}
		if localCount > 0 {
			localityScore := float64(localCount) / float64(len(c.Inputs)) * 5.0
			s.Total += localityScore
			s.Reasons = append(s.Reasons, fmt.Sprintf("%d/%d inputs local", localCount, len(c.Inputs)))
		}

		// Transfer cost penalty for non-local inputs
		if missingCount > 0 && totalMissingBytes > 0 {
			bw := host.NetworkBWBytesPerSec()
			if bw > 0 {
				transferTimeSec := float64(totalMissingBytes) / bw
				penalty := math.Min(transferTimeSec/60.0, 5.0)
				s.Total -= penalty
				s.Reasons = append(s.Reasons, fmt.Sprintf("~%.1fmin transfer for %d missing inputs", transferTimeSec/60.0, missingCount))
			}
		}
	}

	// Soft factor: current utilization (prefer less-loaded hosts)
	if metrics != nil {
		// GPU utilization: up to -3 penalty for fully loaded GPUs
		if metrics.GPUPercent > 0 {
			gpuPenalty := float64(metrics.GPUPercent) / 100.0 * 3.0
			s.Total -= gpuPenalty
			s.Reasons = append(s.Reasons, fmt.Sprintf("GPU %d%% loaded", metrics.GPUPercent))
		}

		// CPU utilization: up to -1 penalty
		if metrics.CPUPercent > 0 {
			cpuPenalty := float64(metrics.CPUPercent) / 100.0 * 1.0
			s.Total -= cpuPenalty
			s.Reasons = append(s.Reasons, fmt.Sprintf("CPU %d%% loaded", metrics.CPUPercent))
		}

		// Queue depth: -0.5 per queued job (up to -3)
		if metrics.QueueDepth > 0 {
			queuePenalty := math.Min(float64(metrics.QueueDepth)*0.5, 3.0)
			s.Total -= queuePenalty
			s.Reasons = append(s.Reasons, fmt.Sprintf("%d jobs queued", metrics.QueueDepth))
		}

		// GPU jobs queued: -0.5 per queued GPU job (up to -2)
		if metrics.GPUJobsQueued > 0 {
			gpuQueuePenalty := math.Min(float64(metrics.GPUJobsQueued)*0.5, 2.0)
			s.Total -= gpuQueuePenalty
			s.Reasons = append(s.Reasons, fmt.Sprintf("%d GPU jobs queued", metrics.GPUJobsQueued))
		}

		// Per-device GPU memory: penalize hosts where matching GPUs lack free VRAM
		if c.GPUClass != "" && len(metrics.GPUDeviceFreeMemMiB) > 0 {
			s.applyPerDeviceGPUMemScoring(host, c, metrics)
		}
	}

	// Soft factor: performance multiplier (penalize slower hosts)
	if c.GPUClass != "" {
		gpuFactor := host.GPUPerformance()
		if gpuFactor < 1.0 {
			penalty := (1.0 - gpuFactor) * 5.0
			s.Total -= penalty
			s.staticPerfPenalty = penalty
			s.Reasons = append(s.Reasons, fmt.Sprintf("GPU perf %.2fx (-%.1f)", gpuFactor, penalty))
		}
	} else {
		cpuFactor := host.CPUPerformance()
		if cpuFactor < 1.0 {
			penalty := (1.0 - cpuFactor) * 3.0
			s.Total -= penalty
			s.staticPerfPenalty = penalty
			s.Reasons = append(s.Reasons, fmt.Sprintf("CPU perf %.2fx (-%.1f)", cpuFactor, penalty))
		}
	}

	// Base score for eligible hosts (ensures non-zero)
	if s.Eligible {
		s.Total += 1
	}

	return s
}

// applyDurationScoring adds a relative bonus to hosts with duration predictions.
// The fastest host gets +3.0, the slowest gets 0, with linear interpolation.
// When a predictor provides a duration, the static cpu_factor/gpu_factor penalty
// is removed for that host since the predictor subsumes it.
func applyDurationScoring(scores []Score, predictions map[string]*JobPrediction) {
	// Collect hosts with duration predictions
	type hostDuration struct {
		index    int
		duration float64
	}
	var durations []hostDuration
	for i := range scores {
		if !scores[i].Eligible {
			continue
		}
		p := predictions[scores[i].Host]
		if p == nil || p.DurationS == nil {
			continue
		}
		durations = append(durations, hostDuration{index: i, duration: *p.DurationS})
	}

	// Need at least 2 hosts with predictions for relative scoring
	if len(durations) < 2 {
		return
	}

	// Find min/max durations
	minDur, maxDur := durations[0].duration, durations[0].duration
	for _, d := range durations[1:] {
		if d.duration < minDur {
			minDur = d.duration
		}
		if d.duration > maxDur {
			maxDur = d.duration
		}
	}

	spread := maxDur - minDur
	if spread <= 0 {
		return
	}

	const maxBonus = 3.0
	for _, d := range durations {
		// Linear: fastest gets maxBonus, slowest gets 0
		bonus := (1.0 - (d.duration-minDur)/spread) * maxBonus
		scores[d.index].Total += bonus

		// Remove the static perf penalty since predictor subsumes it
		removeStaticPerfPenalty(&scores[d.index])

		durMin := d.duration / 60.0
		scores[d.index].Reasons = append(scores[d.index].Reasons,
			fmt.Sprintf("predicted %.0fm (+%.1f)", durMin, bonus))
	}
}

// removeStaticPerfPenalty reverses the cpu_factor/gpu_factor penalty that was
// applied in scoreHost, since the predictor has better per-host data.
func removeStaticPerfPenalty(s *Score) {
	if s.staticPerfPenalty == 0 {
		return
	}
	s.Total += s.staticPerfPenalty
	s.staticPerfPenalty = 0

	// Remove the perf reason from display
	filtered := s.Reasons[:0]
	for _, r := range s.Reasons {
		if !strings.Contains(r, "CPU perf") && !strings.Contains(r, "GPU perf") {
			filtered = append(filtered, r)
		}
	}
	s.Reasons = filtered
}

// applyResourceHardConstraints marks a host ineligible if predicted resource
// usage (95% CI upper bound) exceeds host capacity.
func applyResourceHardConstraints(s *Score, p *JobPrediction, spec inventory.HostSpec) {
	// Check RSS vs total host RAM
	if p.PeakRSSKBUpper != nil {
		hostMemGB := parseMemGB(spec.Memory)
		if hostMemGB > 0 {
			hostMemKB := float64(hostMemGB) * 1024 * 1024 // GB to KB
			if *p.PeakRSSKBUpper > hostMemKB {
				s.Eligible = false
				s.Reasons = append(s.Reasons,
					fmt.Sprintf("predicted RSS ~%.0f GiB exceeds %.0f GB RAM",
						*p.PeakRSSKBUpper/(1024*1024), float64(hostMemGB)))
				return
			}
		}
	}

	// Check GPU mem vs largest GPU on host
	if p.MaxGPUMemMiBUpper != nil && len(spec.GPUs) > 0 {
		var maxGPUMemGB int
		for _, gpu := range spec.GPUs {
			if mem := parseMemGB(gpu.Memory); mem > maxGPUMemGB {
				maxGPUMemGB = mem
			}
		}
		if maxGPUMemGB > 0 {
			maxGPUMemMiB := float64(maxGPUMemGB) * 1024 // GB to MiB (approx)
			if *p.MaxGPUMemMiBUpper > maxGPUMemMiB {
				s.Eligible = false
				s.Reasons = append(s.Reasons,
					fmt.Sprintf("predicted GPU mem ~%.0f GiB exceeds %d GB GPU",
						*p.MaxGPUMemMiBUpper/1024, maxGPUMemGB))
				return
			}
		}
	}
}

// applyResourceSoftConstraints penalizes hosts where predicted usage is close
// to currently available resources.
func applyResourceSoftConstraints(s *Score, p *JobPrediction, m *HostMetrics) {
	if m == nil {
		return
	}

	// RAM headroom: penalize if predicted RSS > 80% of free RAM
	if p.PeakRSSKB != nil && m.FreeRAMKB > 0 {
		ratio := *p.PeakRSSKB / float64(m.FreeRAMKB)
		if ratio > 0.8 {
			penalty := math.Min((ratio-0.8)/0.2*2.0, 2.0)
			s.Total -= penalty
			s.Reasons = append(s.Reasons,
				fmt.Sprintf("tight RAM fit (predicted %.1f GiB, %.1f GiB free)",
					*p.PeakRSSKB/(1024*1024), float64(m.FreeRAMKB)/(1024*1024)))
		}
	}

	// GPU memory headroom
	if p.MaxGPUMemMiB != nil && m.FreeGPUMemMiB > 0 {
		ratio := *p.MaxGPUMemMiB / float64(m.FreeGPUMemMiB)
		if ratio > 0.8 {
			penalty := math.Min((ratio-0.8)/0.2*2.0, 2.0)
			s.Total -= penalty
			s.Reasons = append(s.Reasons,
				fmt.Sprintf("tight GPU mem fit (predicted %.1f GiB, %.1f GiB free)",
					*p.MaxGPUMemMiB/1024, float64(m.FreeGPUMemMiB)/1024))
		}
	}
}

func sortScores(scores []Score) {
	for i := 1; i < len(scores); i++ {
		for j := i; j > 0; j-- {
			if betterThan(scores[j], scores[j-1]) {
				scores[j], scores[j-1] = scores[j-1], scores[j]
			}
		}
	}
}

func betterThan(a, b Score) bool {
	if a.Eligible != b.Eligible {
		return a.Eligible
	}
	return a.Total > b.Total
}

// parseMemGB extracts the GB value from a string like "80GB" or "24GB".
func parseMemGB(s string) int {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "GB")
	s = strings.TrimSuffix(s, "gb")
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// normalizeGPUClass strips spaces, punctuation, and lowercases for fuzzy matching.
// e.g. "RTX 3090", "rtx3090", "rtx-3090" all normalize to "rtx3090".
func normalizeGPUClass(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// applyPerDeviceGPUMemScoring penalizes a host based on how many of its
// matching-class GPUs have enough free VRAM for the job.
func (s *Score) applyPerDeviceGPUMemScoring(host inventory.HostSpec, c Constraints, metrics *HostMetrics) {
	norm := normalizeGPUClass(c.GPUClass)

	// Determine required memory in MiB
	var requiredMiB int64
	if c.GPUMemGB > 0 {
		requiredMiB = int64(c.GPUMemGB) * 1024
	}

	// Count matching-class devices and how many have enough free VRAM
	totalMatching := 0
	withEnough := 0
	for _, gpu := range host.GPUs {
		if normalizeGPUClass(gpu.Class) != norm {
			continue
		}
		for _, idx := range gpu.Indices {
			totalMatching++
			idxStr := fmt.Sprintf("%d", idx)
			freeMiB, ok := metrics.GPUDeviceFreeMemMiB[idxStr]
			if !ok {
				// No data for this device — assume it's available
				withEnough++
				continue
			}
			if requiredMiB == 0 || freeMiB >= requiredMiB {
				withEnough++
			}
		}
	}

	if totalMatching == 0 {
		return
	}

	if withEnough == 0 {
		// No matching device has enough VRAM — strong penalty (defeasible)
		s.Total -= 5.0
		s.Reasons = append(s.Reasons,
			fmt.Sprintf("0/%d %ss have enough free VRAM", totalMatching, c.GPUClass))
	} else if withEnough < totalMatching {
		// Some but not all — moderate penalty scaled by fraction unavailable
		fraction := float64(totalMatching-withEnough) / float64(totalMatching)
		penalty := fraction * 3.0
		s.Total -= penalty
		s.Reasons = append(s.Reasons,
			fmt.Sprintf("%d/%d %ss have enough free VRAM", withEnough, totalMatching, c.GPUClass))
	}
}

func describeConstraints(c Constraints) string {
	var parts []string
	if c.GPUClass != "" {
		parts = append(parts, "gpu-class="+c.GPUClass)
	}
	if c.GPUMemGB > 0 {
		parts = append(parts, fmt.Sprintf("gpu-mem>=%dGB", c.GPUMemGB))
	}
	if len(c.Inputs) > 0 {
		parts = append(parts, fmt.Sprintf("%d inputs", len(c.Inputs)))
	}
	if c.Command != "" {
		cmd := c.Command
		if len(cmd) > 40 {
			cmd = cmd[:37] + "..."
		}
		parts = append(parts, fmt.Sprintf("cmd=%q", cmd))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
