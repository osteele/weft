// Package placement scores hosts for job placement based on resource
// constraints, data locality, and current utilization.
package placement

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/transferbw"
)

// ErrNoReachableHost is returned when all eligible hosts are unreachable.
var ErrNoReachableHost = errors.New("no eligible host is reachable")

// ErrNoEligibleHost is returned when no host in the inventory satisfies the job constraints.
var ErrNoEligibleHost = errors.New("no eligible host found")

var loadHosts = inventory.LoadHosts
var collectMetrics = CollectMetrics

// Constraints describes hard requirements for a job placement.
type Constraints struct {
	GPUClass string   // Required GPU class (e.g., "a100"); empty = no preference
	GPUMemGB int      // Minimum GPU memory in GB; 0 = no minimum
	Inputs   []string // Asset refs the job reads (for locality scoring)
	Command  string   // For predictor-based scoring; empty = skip
	Project  string   // For predictor-based scoring; empty = skip
	Tags     []string // Job tags; "benchmark" triggers idle-host requirement
}

// NeedsGPU returns true if the constraints require GPU resources.
func (c Constraints) NeedsGPU() bool {
	return c.GPUClass != "" || c.GPUMemGB > 0
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
	DurationS      *float64 // predicted wall-clock seconds (nil = unknown)
	DurationSLower *float64 // p10 lower bound
	DurationSUpper *float64 // p90 upper bound
	PeakRSSKB      *float64 // predicted peak RSS in KB (nil = unknown)
	MaxGPUMemMiB   *float64 // predicted peak GPU memory in MiB (nil = unknown)
	// Lower bounds (p10) for optimistic estimates
	PeakRSSKBLower    *float64
	MaxGPUMemMiBLower *float64
	// Upper bounds (p90) for hard constraint checking
	PeakRSSKBUpper    *float64
	MaxGPUMemMiBUpper *float64
}

// JobPredictor returns predicted resource needs for a job on a given host.
// Returns nil if prediction is unavailable.
type JobPredictor func(host string) *JobPrediction

// RawPredictionField holds a point estimate with uncertainty bounds.
type RawPredictionField struct {
	Mean  float64
	Lower float64
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
			jp.DurationSLower = &raw.DurationS.Lower
			jp.DurationSUpper = &raw.DurationS.Upper
		}
		if raw.PeakRSSKB != nil {
			jp.PeakRSSKB = &raw.PeakRSSKB.Mean
			jp.PeakRSSKBLower = &raw.PeakRSSKB.Lower
			jp.PeakRSSKBUpper = &raw.PeakRSSKB.Upper
		}
		if raw.MaxGPUMemMiB != nil {
			jp.MaxGPUMemMiB = &raw.MaxGPUMemMiB.Mean
			jp.MaxGPUMemMiBLower = &raw.MaxGPUMemMiB.Lower
			jp.MaxGPUMemMiBUpper = &raw.MaxGPUMemMiB.Upper
		}
		return jp
	}
}

// PlacementResult holds the full output of a placement decision.
type PlacementResult struct {
	Host    string
	Reasons []string
	Scores  []Score
	Metrics map[string]*HostMetrics // nil when no live metrics were collected
}

// Score represents the placement score for a single host.
type Score struct {
	Host             string
	Total            float64 // Higher is better
	Eligible         bool    // Passes all hard constraints
	Reasons          []string
	staticPerfDelta  float64 // score delta from cpu_factor/gpu_factor, tracked for reversal
	staticPerfReason string  // the exact reason string added with the delta
	queuePenalty     float64 // score delta from queue depth, tracked for MC reversal
	queueReason      string  // the exact reason string added with the penalty
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
	hosts, err := loadHosts()
	if err != nil {
		return nil, fmt.Errorf("load inventory: %w", err)
	}
	scores := ScoreHostListWithMetrics(db, hosts, constraints, metrics)
	return scores, nil
}

// ScoreHostListWithMetrics evaluates the given hosts with optional live utilization data.
func ScoreHostListWithMetrics(db *sql.DB, hosts []inventory.HostSpec, constraints Constraints, metrics map[string]*HostMetrics) []Score {
	scores := scoreAll(db, hosts, constraints, metrics)
	sortScores(scores)
	return scores
}

// ScoreHostsWithPredictor evaluates hosts with optional predictor and metrics.
func ScoreHostsWithPredictor(db *sql.DB, constraints Constraints, metrics map[string]*HostMetrics, predict JobPredictor) ([]Score, error) {
	hosts, err := loadHosts()
	if err != nil {
		return nil, fmt.Errorf("load inventory: %w", err)
	}
	scores := ScoreHostListWithPredictor(db, hosts, constraints, metrics, predict)
	return scores, nil
}

// ScoreHostListWithPredictor evaluates the given hosts with optional predictor and metrics.
func ScoreHostListWithPredictor(db *sql.DB, hosts []inventory.HostSpec, constraints Constraints, metrics map[string]*HostMetrics, predict JobPredictor) []Score {
	scores := scoreAll(db, hosts, constraints, metrics)

	if predict == nil {
		sortScores(scores)
		return scores
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

	// Try Monte Carlo scoring first; fall back to deterministic if <2 hosts have predictions
	estimates := SimulateCompletionTimes(scores, predictions, metrics, DefaultMonteCarloConfig())
	if len(estimates) >= 2 {
		ApplyMonteCarloScoring(scores, estimates)
	} else {
		applyDurationScoring(scores, predictions)
	}

	sortScores(scores)
	return scores
}

// scoreAll runs scoreHost for each host. Does not sort.
func scoreAll(db *sql.DB, hosts []inventory.HostSpec, constraints Constraints, metrics map[string]*HostMetrics) []Score {
	cfg, err := config.Load()
	if err != nil {
		cfg = nil
	}
	scores := make([]Score, 0, len(hosts))
	for _, h := range hosts {
		var m *HostMetrics
		if metrics != nil {
			m = metrics[h.Name]
		}
		scores = append(scores, scoreHost(db, h, constraints, m, cfg))
	}
	return scores
}

// BestHostWithPredictor returns the best eligible host considering predictor and metrics.
func BestHostWithPredictor(db *sql.DB, constraints Constraints, metrics map[string]*HostMetrics, predict JobPredictor) (*PlacementResult, error) {
	scores, err := ScoreHostsWithPredictor(db, constraints, metrics, predict)
	if err != nil {
		return nil, err
	}
	return bestFromScores(scores, constraints, metrics)
}

// BestHost returns the best eligible host, or an error if none qualify.
func BestHost(db *sql.DB, constraints Constraints) (*PlacementResult, error) {
	return BestHostWithMetrics(db, constraints, nil)
}

// BestHostWithMetrics returns the best eligible host considering live metrics.
func BestHostWithMetrics(db *sql.DB, constraints Constraints, metrics map[string]*HostMetrics) (*PlacementResult, error) {
	scores, err := ScoreHostsWithMetrics(db, constraints, metrics)
	if err != nil {
		return nil, err
	}
	return bestFromScores(scores, constraints, metrics)
}

func bestFromScores(scores []Score, constraints Constraints, metrics map[string]*HostMetrics) (*PlacementResult, error) {
	for _, s := range scores {
		if s.Eligible {
			return &PlacementResult{
				Host:    s.Host,
				Reasons: s.Reasons,
				Scores:  scores,
				Metrics: metrics,
			}, nil
		}
	}
	return nil, fmt.Errorf("no eligible host found for constraints: %s: %w", DescribeConstraints(constraints), ErrNoEligibleHost)
}

// BestReachableHost returns the best eligible host that is also reachable via SSH.
// It collects live metrics from eligible hosts (which also proves reachability),
// scores them with utilization data, and returns the highest-scored reachable host.
// Returns ErrNoReachableHost if no eligible host responds.
func BestReachableHost(db *sql.DB, constraints Constraints, probeTimeout time.Duration) (*PlacementResult, error) {
	return BestReachableHostWithPredictor(db, constraints, probeTimeout, nil)
}

// BestReachableHostWithPredictor returns the best eligible reachable host using
// live metrics and optional predictor-based resource scoring.
func BestReachableHostWithPredictor(db *sql.DB, constraints Constraints, probeTimeout time.Duration, predict JobPredictor) (*PlacementResult, error) {
	// First pass: static scoring to determine eligible hosts
	hosts, err := loadHosts()
	if err != nil {
		return nil, fmt.Errorf("load inventory: %w", err)
	}
	staticScores := scoreAll(db, hosts, constraints, nil)
	var eligible []string
	for _, s := range staticScores {
		if s.Eligible {
			eligible = append(eligible, s.Host)
		}
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no eligible host found for constraints: %s: %w", DescribeConstraints(constraints), ErrNoEligibleHost)
	}

	// Collect live metrics (also proves reachability)
	metrics := collectMetrics(db, eligible, probeTimeout)
	if len(metrics) == 0 {
		return nil, ErrNoReachableHost
	}

	// Log host metrics to oplog for offline analysis
	for _, host := range eligible {
		if m, ok := metrics[host]; ok {
			oplog.Log(oplog.OpHostMetrics,
				oplog.WithHost(host),
				oplog.WithDetail(FormatHostMetricsDetail(m)))
		}
	}

	reachableHosts := make([]inventory.HostSpec, 0, len(hosts))
	for _, h := range hosts {
		if metrics[h.Name] != nil {
			reachableHosts = append(reachableHosts, h)
		}
	}
	if len(reachableHosts) == 0 {
		return nil, ErrNoReachableHost
	}

	scores := ScoreHostListWithPredictor(db, reachableHosts, constraints, metrics, predict)

	for _, s := range scores {
		if s.Eligible && metrics[s.Host] != nil {
			return &PlacementResult{
				Host:    s.Host,
				Reasons: s.Reasons,
				Scores:  scores,
				Metrics: metrics,
			}, nil
		}
	}

	return nil, ErrNoReachableHost
}

// PlaceWithFallback tries live placement first, falls back to static scoring,
// and returns ErrNoEligibleHost if no host matches at all.
// This encapsulates the common BestReachableHost → BestHostWithPredictor fallback chain.
func PlaceWithFallback(database *sql.DB, constraints Constraints, predict JobPredictor) (*PlacementResult, error) {
	// Rental-tagged jobs skip local placement entirely.
	if db.HasRentalTag(constraints.Tags) {
		return nil, ErrNoEligibleHost
	}

	result, err := BestReachableHostWithPredictor(database, constraints, 5*time.Second, predict)
	if err == nil {
		return result, nil
	}
	if errors.Is(err, ErrNoReachableHost) {
		return BestHostWithPredictor(database, constraints, nil, predict)
	}
	return nil, err
}

func scoreHost(database *sql.DB, host inventory.HostSpec, c Constraints, metrics *HostMetrics, cfg *config.Config) Score {
	s := Score{Host: host.Name, Eligible: true}

	// Hard constraint: GPU class (supports exact model, generation, or minimum generation)
	var gc GPUConstraint
	if c.GPUClass != "" {
		gc = ParseGPUConstraint(c.GPUClass)
		var matchedName string
		for _, gpu := range host.GPUs {
			if gc.MatchesGPU(gpu.Class) {
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

	// Hard constraint: benchmark jobs require an idle host
	if hasBenchmarkTag(c.Tags) {
		if !db.HasInventoryTag(c.Tags) && cfg != nil && cfg.HostShared(host.Name) {
			s.Eligible = false
			s.Reasons = append(s.Reasons, "shared host excluded for benchmark auto-placement")
			return s
		}
		reason := benchmarkIdleCheck(database, host.Name, metrics)
		if reason != "" {
			s.Eligible = false
			s.Reasons = append(s.Reasons, reason)
			return s
		}
		s.Reasons = append(s.Reasons, "host idle (benchmark)")
	}

	// Soft factor: data locality + transfer cost
	if database != nil && len(c.Inputs) > 0 {
		localCount := 0
		var totalMissingBytes int64
		missingCount := 0
		for _, ref := range c.Inputs {
			asset, ok := dataloc.ParseAssetRef(ref)
			if !ok {
				continue
			}
			entries, err := dataloc.FindAssetHosts(database, asset)
			if err != nil {
				log.Printf("placement: failed to find asset hosts for %v: %v", asset, err)
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
			staticBW := host.NetworkBWBytesPerSec()
			destKey := transferbw.OnPremEndpoint(host.Name).Key()
			effectiveBW, nObs := transferbw.EffectiveBandwidthToDest(database, destKey, staticBW)
			if effectiveBW > 0 {
				transferTimeSec := float64(totalMissingBytes) / effectiveBW
				penalty := math.Min(transferTimeSec/60.0, 5.0)
				s.Total -= penalty
				bwSource := "static"
				if nObs >= transferbw.MinObservations {
					bwSource = fmt.Sprintf("learned, n=%d", nObs)
				}
				s.Reasons = append(s.Reasons, fmt.Sprintf("~%.1fmin transfer for %d missing inputs (%s)", transferTimeSec/60.0, missingCount, bwSource))
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
			s.queuePenalty = queuePenalty
			reason := fmt.Sprintf("%d jobs queued", metrics.QueueDepth)
			s.queueReason = reason
			s.Reasons = append(s.Reasons, reason)
		}

		// GPU jobs queued: -0.5 per queued GPU job (up to -2)
		if metrics.GPUJobsQueued > 0 {
			gpuQueuePenalty := math.Min(float64(metrics.GPUJobsQueued)*0.5, 2.0)
			s.Total -= gpuQueuePenalty
			s.Reasons = append(s.Reasons, fmt.Sprintf("%d GPU jobs queued", metrics.GPUJobsQueued))
		}

		// Per-device GPU memory: penalize hosts where matching GPUs lack free VRAM
		if c.NeedsGPU() && len(metrics.GPUDeviceFreeMemMiB) > 0 {
			s.applyPerDeviceGPUMemScoring(host, c, metrics.GPUDeviceFreeMemMiB, gc)
		}
	}

	// Soft factor: performance multiplier (weighted by cpu_factor or gpu_factor)
	if c.NeedsGPU() {
		applyPerfScoring(&s, "GPU", host.GPUPerformance(), 5.0)
	} else {
		applyPerfScoring(&s, "CPU", host.CPUPerformance(), 3.0)
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
		rawBonus := (1.0 - (d.duration-minDur)/spread) * maxBonus

		// Scale bonus by confidence: narrow CI → full bonus, wide CI → reduced bonus
		p := predictions[scores[d.index].Host]
		weight := 1.0
		if p.DurationSLower != nil && p.DurationSUpper != nil && d.duration > 0 {
			intervalRatio := (*p.DurationSUpper - *p.DurationSLower) / d.duration
			weight = math.Max(0, math.Min(1, 1-0.25*intervalRatio))
		}
		bonus := rawBonus * weight

		scores[d.index].Total += bonus

		// Remove the static perf scoring since predictor subsumes it
		removeStaticPerfScoring(&scores[d.index])

		durMin := d.duration / 60.0
		if p.DurationSLower != nil && p.DurationSUpper != nil {
			halfSpread := (d.duration - *p.DurationSLower) / 60.0
			scores[d.index].Reasons = append(scores[d.index].Reasons,
				fmt.Sprintf("predicted %.0fm (\u00b1%.0fm, +%.1f)", durMin, halfSpread, bonus))
		} else {
			scores[d.index].Reasons = append(scores[d.index].Reasons,
				fmt.Sprintf("predicted %.0fm (+%.1f)", durMin, bonus))
		}
	}
}

// applyPerfScoring adds a score contribution proportional to the host's
// performance factor. The weight controls how much performance matters
// relative to other scoring factors.
func applyPerfScoring(s *Score, label string, factor float64, weight float64) {
	score := factor * weight
	s.Total += score
	s.staticPerfDelta = score
	if factor != 1.0 {
		reason := fmt.Sprintf("%s perf %.2fx (%+.1f)", label, factor, score)
		s.staticPerfReason = reason
		s.Reasons = append(s.Reasons, reason)
	}
}

// removeStaticPerfScoring reverses the cpu_factor/gpu_factor contribution that
// was applied in scoreHost, since the predictor has better per-host data.
func removeStaticPerfScoring(s *Score) {
	if s.staticPerfDelta == 0 {
		return
	}
	s.Total -= s.staticPerfDelta
	s.staticPerfDelta = 0

	// Remove the stored perf reason from display
	if s.staticPerfReason != "" {
		filtered := s.Reasons[:0]
		for _, r := range s.Reasons {
			if r != s.staticPerfReason {
				filtered = append(filtered, r)
			}
		}
		s.Reasons = filtered
		s.staticPerfReason = ""
	}
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
	// Prefer upper bound for conservative estimate; fall back to mean
	rssEstimate := p.PeakRSSKBUpper
	if rssEstimate == nil {
		rssEstimate = p.PeakRSSKB
	}
	if rssEstimate != nil && m.FreeRAMKB > 0 {
		ratio := *rssEstimate / float64(m.FreeRAMKB)
		if ratio > 0.8 {
			penalty := math.Min((ratio-0.8)/0.2*2.0, 2.0)
			s.Total -= penalty
			s.Reasons = append(s.Reasons,
				fmt.Sprintf("tight RAM fit (predicted %.1f GiB, %.1f GiB free)",
					*rssEstimate/(1024*1024), float64(m.FreeRAMKB)/(1024*1024)))
		}
	}

	// GPU memory headroom
	// Prefer upper bound for conservative estimate; fall back to mean
	gpuEstimate := p.MaxGPUMemMiBUpper
	if gpuEstimate == nil {
		gpuEstimate = p.MaxGPUMemMiB
	}
	if gpuEstimate != nil && m.FreeGPUMemMiB > 0 {
		ratio := *gpuEstimate / float64(m.FreeGPUMemMiB)
		if ratio > 0.8 {
			penalty := math.Min((ratio-0.8)/0.2*2.0, 2.0)
			s.Total -= penalty
			s.Reasons = append(s.Reasons,
				fmt.Sprintf("tight GPU mem fit (predicted %.1f GiB, %.1f GiB free)",
					*gpuEstimate/1024, float64(m.FreeGPUMemMiB)/1024))
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

// normalizeGPUClass is a package-local alias for inventory.NormalizeGPUClass.
var normalizeGPUClass = inventory.NormalizeGPUClass

// applyPerDeviceGPUMemScoring penalizes a host based on how many of its
// matching-class GPUs have enough free VRAM for the job.
func (s *Score) applyPerDeviceGPUMemScoring(host inventory.HostSpec, c Constraints, deviceFreeMem map[string]int64, gc GPUConstraint) {

	// Determine required memory in MiB
	var requiredMiB int64
	if c.GPUMemGB > 0 {
		requiredMiB = int64(c.GPUMemGB) * 1024
	}

	// Count matching-class devices and how many have enough free VRAM
	totalMatching := 0
	withEnough := 0
	for _, gpu := range host.GPUs {
		if !gc.MatchesGPU(gpu.Class) {
			continue
		}
		for _, idx := range gpu.Indices {
			totalMatching++
			freeMiB, ok := deviceFreeMem[strconv.Itoa(idx)]
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

func DescribeConstraints(c Constraints) string {
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
	if hasBenchmarkTag(c.Tags) {
		parts = append(parts, "benchmark")
	}
	if db.HasRentalTag(c.Tags) {
		parts = append(parts, db.TagRental)
	}
	if db.HasInventoryTag(c.Tags) {
		parts = append(parts, db.TagInventory)
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

// ExplainUnplaced returns compact user-facing reasons for why local placement
// left a job unplaced.
func ExplainUnplaced(database *sql.DB, constraints Constraints) ([]string, error) {
	if db.HasRentalTag(constraints.Tags) {
		return []string{"rental-tagged job skips local placement"}, nil
	}

	scores, err := ScoreHostsWithMetrics(database, constraints, nil)
	if err != nil {
		return nil, err
	}

	type reasonGroup struct {
		reason string
		hosts  []string
	}
	reasonHosts := make(map[string][]string)
	for _, score := range scores {
		if score.Eligible {
			continue
		}
		reason := "host did not match constraints"
		if len(score.Reasons) > 0 && score.Reasons[0] != "" {
			reason = score.Reasons[0]
		}
		reasonHosts[reason] = append(reasonHosts[reason], score.Host)
	}

	reasons := []string{fmt.Sprintf("no local host matched %s", DescribeConstraints(constraints))}

	if len(reasonHosts) > 0 {
		groups := make([]reasonGroup, 0, len(reasonHosts))
		for reason, hosts := range reasonHosts {
			slices.Sort(hosts)
			groups = append(groups, reasonGroup{reason: reason, hosts: hosts})
		}
		sort.Slice(groups, func(i, j int) bool {
			if len(groups[i].hosts) != len(groups[j].hosts) {
				return len(groups[i].hosts) > len(groups[j].hosts)
			}
			return groups[i].reason < groups[j].reason
		})
		for i, group := range groups {
			if i >= 2 {
				break
			}
			reasons = append(reasons, fmt.Sprintf("%d host%s: %s", len(group.hosts), pluralSuffix(len(group.hosts)), group.reason))
		}
	}

	if db.HasInventoryTag(constraints.Tags) {
		reasons = append(reasons, "inventory-tagged job will not use rental GPUs")
	}
	return reasons, nil
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// FormatPlacementDetail builds a compact detail string for oplog entries.
func FormatPlacementDetail(result *PlacementResult) string {
	if result == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("selected=")
	b.WriteString(result.Host)

	// Scores
	if len(result.Scores) > 0 {
		b.WriteString(" scores=")
		for i, s := range result.Scores {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%s:%.1f", s.Host, s.Total)
			if !s.Eligible {
				b.WriteString("(x)")
			}
		}
	}

	// Metrics summary (sorted for deterministic output)
	if len(result.Metrics) > 0 {
		metricHosts := make([]string, 0, len(result.Metrics))
		for host := range result.Metrics {
			metricHosts = append(metricHosts, host)
		}
		sort.Strings(metricHosts)
		b.WriteString(" metrics=")
		for i, host := range metricHosts {
			if i > 0 {
				b.WriteString(",")
			}
			m := result.Metrics[host]
			fmt.Fprintf(&b, "%s:{cpu:%d,gpu:%d,q:%d}", host, m.CPUPercent, m.GPUPercent, m.QueueDepth)
		}
	}

	return b.String()
}

// FormatHostMetricsDetail builds a compact detail string for a single host's metrics oplog entry.
func FormatHostMetricsDetail(m *HostMetrics) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "cpu=%d gpu=%d ram=%d q=%d", m.CPUPercent, m.GPUPercent, m.RAMPercent, m.QueueDepth)
	if len(m.GPUDeviceFreeMemMiB) > 0 {
		indices := make([]string, 0, len(m.GPUDeviceFreeMemMiB))
		for idx := range m.GPUDeviceFreeMemMiB {
			indices = append(indices, idx)
		}
		sort.Strings(indices)
		b.WriteString(" gpu_free=")
		for i, idx := range indices {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%s:%d", idx, m.GPUDeviceFreeMemMiB[idx])
		}
	}
	return b.String()
}

// Benchmark idle thresholds — reads the same env vars as runner.DefaultBenchmarkConfig()
// to stay in sync without creating an import cycle.
var (
	benchmarkCPUThreshold = intFromEnvOrDefault("WEFT_BENCHMARK_CPU", 5)
	benchmarkGPUThreshold = intFromEnvOrDefault("WEFT_BENCHMARK_GPU", 5)
	benchmarkRAMThreshold = intFromEnvOrDefault("WEFT_BENCHMARK_RAM", 20)
)

func intFromEnvOrDefault(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

// hasBenchmarkTag returns true if tags contain the benchmark tag.
func hasBenchmarkTag(tags []string) bool {
	return slices.Contains(tags, db.TagBenchmark)
}

// benchmarkIdleCheck returns a non-empty reason string if the host is not idle
// enough for a benchmark job. Returns "" if the host is idle.
func benchmarkIdleCheck(database *sql.DB, host string, metrics *HostMetrics) string {
	if metrics == nil {
		return "no live metrics (benchmark requires idle verification)"
	}

	// Check for active weft jobs on this host
	if database != nil {
		active, err := db.CountQueueRunnerActiveByHost(database, host)
		if err == nil && active > 0 {
			return fmt.Sprintf("%d active weft jobs (benchmark requires idle host)", active)
		}
	}

	if metrics.CPUPercent > benchmarkCPUThreshold {
		return fmt.Sprintf("CPU %d%% > %d%% (benchmark requires idle host)", metrics.CPUPercent, benchmarkCPUThreshold)
	}
	if metrics.GPUPercent > benchmarkGPUThreshold {
		return fmt.Sprintf("GPU %d%% > %d%% (benchmark requires idle host)", metrics.GPUPercent, benchmarkGPUThreshold)
	}
	if metrics.RAMPercent > benchmarkRAMThreshold {
		return fmt.Sprintf("RAM %d%% > %d%% (benchmark requires idle host)", metrics.RAMPercent, benchmarkRAMThreshold)
	}

	return ""
}
