// Package placement scores hosts for job placement based on resource
// constraints, data locality, and current utilization.
package placement

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/osteele/weft/internal/estimate"
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

// LoadHostNames returns the names of all inventory hosts.
func LoadHostNames() ([]string, error) {
	hosts, err := loadHosts()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.Name
	}
	return names, nil
}

// Constraints describes hard requirements for a job placement.
type Constraints struct {
	GPUClass string   // Required GPU class (e.g., "a100"); empty = no preference
	Provider string   // Requested rental provider (e.g., "vastai", "runpod"); empty = any
	GPUMemGB int      // Minimum GPU memory in GB; 0 = no minimum
	Inputs   []string // Asset refs the job reads (for locality scoring)
	Command  string   // For predictor-based scoring; empty = skip
	Project  string   // For predictor-based scoring; empty = skip
	Tags     []string // Job tags; "benchmark" triggers idle-host requirement

	// PreferredInstanceIDs is a soft preference toward reusing these specific
	// rental instances. Used to co-locate a consumer on its --needs
	// producer's rental instance so the agent can read outputs from the
	// shared workdir instead of staging from R2. The preference is a
	// tie-breaker / tip: if the preferred instance fails the GPU class /
	// capacity / provider filters, placement falls through to the normal
	// ranking.
	PreferredInstanceIDs []int64

	// MaxComputeCap is the highest CUDA compute capability the job's
	// installed PyTorch wheel can target ("9.0", "12.0", ...). Empty string
	// disables this filter. Inferred from the project's uv.lock or
	// pyproject.toml via dataloc.ScanTorchPin and TorchMaxComputeCap, or
	// supplied explicitly via [tool.weft] gpu-arch-max.
	MaxComputeCap string
}

// ConstraintsFromJob builds Constraints from a db.Job's fields.
func ConstraintsFromJob(j *db.Job) Constraints {
	c := Constraints{
		GPUClass: j.GPUClass,
		Inputs:   j.Inputs,
		Command:  j.Command,
		Project:  j.Project,
		Tags:     j.Tags,
	}
	if provider, ok := db.RequestedProvider(j.Tags); ok {
		c.Provider = provider
	}
	if j.GPUMemGB != nil {
		c.GPUMemGB = *j.GPUMemGB
	}
	return c
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

	// Completion estimate for the placed host (zero if unavailable).
	CompletionEst estimate.Estimate

	// Spill info: set when on-prem was rejected in favor of rental.
	SpilledToRental bool
	RentalEst       *estimate.Estimate // estimated rental completion time
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

	// Completion time estimates (zero means unavailable).
	// These decompose estimated time-to-completion into phases.
	QueueDrainEst    estimate.Estimate // time for queued jobs ahead to finish
	TransferEst      estimate.Estimate // data transfer time for missing inputs
	RunEst           estimate.Estimate // job execution time on this host
	CompletionEst    estimate.Estimate // total: queue + transfer + run
	ContentionFactor float64           // multiplier applied to run time (1.0 = no contention)
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
				Host:          s.Host,
				Reasons:       s.Reasons,
				Scores:        scores,
				Metrics:       metrics,
				CompletionEst: s.CompletionEst,
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
	return bestReachableHostWithPredictor(db, constraints, probeTimeout, predict, nil)
}

// bestReachableHostWithPredictor is the internal implementation that accepts
// optional pre-collected metrics to avoid redundant SSH probes when placing
// multiple jobs in a single cycle.
func bestReachableHostWithPredictor(db *sql.DB, constraints Constraints, probeTimeout time.Duration, predict JobPredictor, preMetrics map[string]*HostMetrics) (*PlacementResult, error) {
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

	// Use pre-collected metrics if available, otherwise probe via SSH.
	var metrics map[string]*HostMetrics
	if preMetrics != nil {
		metrics = make(map[string]*HostMetrics, len(eligible))
		for _, h := range eligible {
			if m, ok := preMetrics[h]; ok {
				metrics[h] = m
			}
		}
	} else {
		metrics = collectMetrics(db, eligible, probeTimeout)
	}
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
				Host:          s.Host,
				Reasons:       s.Reasons,
				Scores:        scores,
				Metrics:       metrics,
				CompletionEst: s.CompletionEst,
			}, nil
		}
	}

	return nil, ErrNoReachableHost
}

// PlaceOnPrem tries live on-prem placement first, falls back to static scoring.
// Returns ErrNoEligibleHost if no inventory host matches.
// Does not compare against rental — use Evaluate for cross-strategy comparison.
func PlaceOnPrem(database *sql.DB, constraints Constraints, predict JobPredictor) (*PlacementResult, error) {
	return placeOnPremWithMetrics(database, constraints, predict, nil)
}

// placeOnPremWithMetrics is like PlaceOnPrem but accepts optional pre-collected
// metrics to avoid redundant SSH probes.
func placeOnPremWithMetrics(database *sql.DB, constraints Constraints, predict JobPredictor, preMetrics map[string]*HostMetrics) (*PlacementResult, error) {
	if db.HasRentalTag(constraints.Tags) || constraints.Provider != "" {
		return nil, ErrNoEligibleHost
	}

	result, err := bestReachableHostWithPredictor(database, constraints, 5*time.Second, predict, preMetrics)
	if err == nil {
		return result, nil
	}
	if errors.Is(err, ErrNoReachableHost) {
		return BestHostWithPredictor(database, constraints, nil, predict)
	}
	return nil, err
}

// PlaceWithFallback tries on-prem placement, then compares against rental
// completion time. If rental is faster, returns spill info alongside ErrNoEligibleHost.
//
// Deprecated: callers should migrate to Evaluate for unified cross-strategy placement.
func PlaceWithFallback(database *sql.DB, constraints Constraints, predict JobPredictor) (*PlacementResult, error) {
	result, err := PlaceOnPrem(database, constraints, predict)
	if err != nil {
		return nil, err
	}

	// Compare on-prem completion time against rental estimate.
	if result.CompletionEst.Mean > 0 && !db.HasInventoryTag(constraints.Tags) {
		rentalEst := EstimateRentalCompletion(database, constraints, predict)
		if rentalEst != nil && rentalEst.Total.Mean > 0 && rentalEst.Total.Mean < result.CompletionEst.Mean {
			slog.Info("rental faster than on-prem, spilling to rental",
				"host", result.Host,
				"onprem_min", fmt.Sprintf("%.0f", result.CompletionEst.Mean.Minutes()),
				"rental_min", fmt.Sprintf("%.0f", rentalEst.Total.Mean.Minutes()))
			result.SpilledToRental = true
			totalEst := rentalEst.Total
			result.RentalEst = &totalEst
			return result, ErrNoEligibleHost
		}
	}
	return result, nil
}

func scoreHost(database *sql.DB, host inventory.HostSpec, c Constraints, metrics *HostMetrics, cfg *config.Config) Score {
	s := Score{Host: host.Name, Eligible: true}

	// Hard constraint: opt-in-only hosts are skipped by auto-placement.
	// They remain usable via an explicit --host, which bypasses the scorer.
	if cfg != nil && cfg.HostOptInOnly(host.Name) {
		s.Eligible = false
		s.Reasons = append(s.Reasons, "host is opt-in only (specify with --host)")
		return s
	}

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
		s.Reasons = append(s.Reasons, fmt.Sprintf("has %s GPU", matchedName))
	}

	// Hard constraint: GPU compute capability upper bound (from torch pin /
	// gpu-arch-max). A host is eligible if at least one of its GPUs has a
	// known cap at or below the bound. GPUs with unknown caps are accepted
	// (we cannot prove they violate).
	if c.MaxComputeCap != "" && !hostHasGPUWithinCap(host, c.MaxComputeCap) {
		s.Eligible = false
		s.Reasons = append(s.Reasons, fmt.Sprintf("no GPU with compute cap <= %s", c.MaxComputeCap))
		return s
	}

	// Hard constraint: GPU memory
	if c.GPUMemGB > 0 {
		found := false
		for _, gpu := range host.GPUs {
			memGB := inventory.ParseMemGB(gpu.Memory)
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

	// --- Time-based scoring ---
	// Compute estimated completion time as: queue drain + data transfer + job run.
	// Score = -completion_time_minutes (lower time = higher score).

	// Transfer time estimate: how long to download missing inputs
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
				slog.Warn("failed to find asset hosts", "component", "placement", "asset", asset, "error", err)
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
			s.Reasons = append(s.Reasons, fmt.Sprintf("%d/%d inputs local", localCount, len(c.Inputs)))
		}

		if totalMissingBytes > 0 {
			staticBW := host.NetworkBWBytesPerSec()
			destKey := transferbw.OnPremEndpoint(host.Name).Key()
			effectiveBW, nObs := transferbw.EffectiveBandwidthToDest(database, destKey, staticBW)
			if effectiveBW > 0 {
				s.TransferEst = estimate.TransferTime(totalMissingBytes, effectiveBW)
				bwSource := "static"
				if nObs >= transferbw.MinObservations {
					bwSource = fmt.Sprintf("learned, n=%d", nObs)
				}
				s.Reasons = append(s.Reasons, fmt.Sprintf("~%.0fm transfer for %d missing inputs (%s)",
					s.TransferEst.Mean.Minutes(), missingCount, bwSource))
			}
		}
	}

	// Queue drain estimate: how long until queued jobs finish
	if metrics != nil {
		queuedJobs := metrics.GPUJobsQueued
		if queuedJobs == 0 {
			queuedJobs = metrics.QueueDepth
		}
		if queuedJobs > 0 {
			// Default: 30 min per queued job (same as MC default)
			const defaultJobDurationMin = 30.0
			meanMin := float64(queuedJobs) * defaultJobDurationMin
			// Bounds: 0.5x to 1.5x per-job uncertainty compounded across queue
			s.QueueDrainEst = estimate.FromSeconds(
				meanMin*60,
				meanMin*60*0.5,
				meanMin*60*1.5,
			)
			s.queuePenalty = s.QueueDrainEst.Mean.Minutes()
			reason := fmt.Sprintf("%d jobs queued (~%.0fm drain)", queuedJobs, s.QueueDrainEst.Mean.Minutes())
			s.queueReason = reason
			s.Reasons = append(s.Reasons, reason)
		}
	}

	// Run time estimate: default 1hr, refined later by predictor/MC
	const defaultRunMin = 60.0
	s.RunEst = estimate.FromSeconds(defaultRunMin*60, defaultRunMin*60*0.25, defaultRunMin*60*4.0)

	// Contention factor: inflate run time by GPU occupancy
	s.ContentionFactor = ContentionFactor(database, host.Name, metrics)
	if s.ContentionFactor > 1.0 {
		s.RunEst = s.RunEst.Scale(s.ContentionFactor)
		s.Reasons = append(s.Reasons, fmt.Sprintf("%.0f%% contention overhead", (s.ContentionFactor-1.0)*100))
	}

	// Performance factor: scale run time by GPU/CPU performance.
	// Compute-intensive jobs skip this — they get capacity-based scoring later.
	if !hasComputeIntensiveTag(c.Tags) {
		if c.NeedsGPU() {
			perfFactor := host.GPUPerformance()
			if perfFactor > 0 && perfFactor != 1.0 {
				s.RunEst = s.RunEst.Scale(1.0 / perfFactor)
				s.staticPerfDelta = perfFactor
				s.staticPerfReason = fmt.Sprintf("GPU perf %.1fx", perfFactor)
				s.Reasons = append(s.Reasons, s.staticPerfReason)
			}
		} else {
			perfFactor := host.CPUPerformance()
			if perfFactor > 0 && perfFactor != 1.0 {
				s.RunEst = s.RunEst.Scale(1.0 / perfFactor)
				s.staticPerfDelta = perfFactor
				s.staticPerfReason = fmt.Sprintf("CPU perf %.1fx", perfFactor)
				s.Reasons = append(s.Reasons, s.staticPerfReason)
			}
		}
	}

	// Assemble completion estimate and set Total score
	s.CompletionEst = s.QueueDrainEst.Add(s.TransferEst).Add(s.RunEst)
	if s.Eligible {
		s.Total = -s.CompletionEst.Mean.Minutes()
		s.Reasons = append(s.Reasons,
			fmt.Sprintf("est. ~%.0fm total (%.0fm queue + %.0fm transfer + %.0fm run)",
				s.CompletionEst.Mean.Minutes(),
				s.QueueDrainEst.Mean.Minutes(),
				s.TransferEst.Mean.Minutes(),
				s.RunEst.Mean.Minutes()))
	}

	// Additive adjustments applied AFTER time-based Total:

	// Per-device GPU memory: penalize hosts where matching GPUs lack free VRAM
	if metrics != nil && c.NeedsGPU() && len(metrics.GPUDeviceFreeMemMiB) > 0 {
		s.applyPerDeviceGPUMemScoring(host, c, metrics.GPUDeviceFreeMemMiB, gc)
	}

	// Compute-intensive: capacity-based bonus
	if hasComputeIntensiveTag(c.Tags) {
		applyComputeIntensiveScoring(&s, host, metrics)
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
		hostMemGB := inventory.ParseMemGB(spec.Memory)
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
			if mem := inventory.ParseMemGB(gpu.Memory); mem > maxGPUMemGB {
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
	if c.Provider != "" {
		parts = append(parts, "provider="+c.Provider)
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
	if constraints.Provider != "" {
		return []string{fmt.Sprintf("provider=%s skips local placement", constraints.Provider)}, nil
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

// hasComputeIntensiveTag returns true if tags contain the compute-intensive tag.
func hasComputeIntensiveTag(tags []string) bool {
	return slices.Contains(tags, db.TagComputeIntensive)
}

// applyComputeIntensiveScoring adds a bonus proportional to the host's effective
// free compute capacity: CPUCores × CPUFactor × idle fraction. With live metrics,
// hosts running GPU workloads that consume CPU are naturally penalized. Without
// metrics, raw capacity (cores × factor) is used.
func applyComputeIntensiveScoring(s *Score, host inventory.HostSpec, metrics *HostMetrics) {
	capacity := float64(host.CPUCores) * host.CPUPerformance()
	if metrics != nil && metrics.CPUPercent > 0 {
		idleFraction := 1.0 - float64(metrics.CPUPercent)/100.0
		effective := capacity * idleFraction
		// Normalize: bonus of up to +5 scaled by effective capacity.
		// 100 effective cores = +5, linearly down.
		bonus := math.Min(effective/100.0*5.0, 5.0)
		s.Total += bonus
		s.Reasons = append(s.Reasons,
			fmt.Sprintf("compute-intensive: %.0f effective cores (%.0f × %.0f%% idle, +%.1f)",
				effective, capacity, idleFraction*100, bonus))
	} else {
		bonus := math.Min(capacity/100.0*5.0, 5.0)
		s.Total += bonus
		s.Reasons = append(s.Reasons,
			fmt.Sprintf("compute-intensive: %.0f capacity (%.0f cores × %.2fx, +%.1f)",
				capacity, float64(host.CPUCores), host.CPUPerformance(), bonus))
	}
}

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
