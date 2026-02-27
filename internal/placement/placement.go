// Package placement scores hosts for job placement based on resource
// constraints, data locality, and current utilization.
package placement

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/inventory"
)

// Constraints describes hard requirements for a job placement.
type Constraints struct {
	GPUClass string   // Required GPU class (e.g., "a100"); empty = no preference
	GPUMemGB int      // Minimum GPU memory in GB; 0 = no minimum
	Inputs   []string // Asset refs the job reads (for locality scoring)
}

// HostMetrics holds live utilization data for a host, used for soft scoring.
// All fields are optional; -1 or 0 means "unknown/unavailable".
type HostMetrics struct {
	CPUPercent int // 0-100, from load average / core count
	GPUPercent int // 0-100, max utilization across GPUs
	RAMPercent int // 0-100
	QueueDepth int // number of queued (pending) jobs
}

// Score represents the placement score for a single host.
type Score struct {
	Host     string
	Total    float64 // Higher is better
	Eligible bool    // Passes all hard constraints
	Reasons  []string
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

	var scores []Score
	for _, h := range hosts {
		var m *HostMetrics
		if metrics != nil {
			m = metrics[h.Name]
		}
		score := scoreHost(db, h, constraints, m)
		scores = append(scores, score)
	}

	// Sort: eligible first, then by score descending
	sortScores(scores)
	return scores, nil
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
	for _, s := range scores {
		if s.Eligible {
			return s.Host, s.Reasons, nil
		}
	}
	return "", nil, fmt.Errorf("no eligible host found for constraints: %s", describeConstraints(constraints))
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
	}

	// Soft factor: performance multiplier (penalize slower hosts)
	if c.GPUClass != "" {
		gpuFactor := host.GPUPerformance()
		if gpuFactor < 1.0 {
			penalty := (1.0 - gpuFactor) * 5.0
			s.Total -= penalty
			s.Reasons = append(s.Reasons, fmt.Sprintf("GPU perf %.2fx (-%.1f)", gpuFactor, penalty))
		}
	} else {
		cpuFactor := host.CPUPerformance()
		if cpuFactor < 1.0 {
			penalty := (1.0 - cpuFactor) * 3.0
			s.Total -= penalty
			s.Reasons = append(s.Reasons, fmt.Sprintf("CPU perf %.2fx (-%.1f)", cpuFactor, penalty))
		}
	}

	// Base score for eligible hosts (ensures non-zero)
	if s.Eligible {
		s.Total += 1
	}

	return s
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
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
