// Package placement scores hosts for job placement based on resource
// constraints, data locality, and current utilization.
package placement

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/inventory"
)

// Constraints describes hard requirements for a job placement.
type Constraints struct {
	GPUClass string   // Required GPU class (e.g., "a100"); empty = no preference
	GPUMemGB int      // Minimum GPU memory in GB; 0 = no minimum
	Inputs   []string // Asset refs the job reads (for locality scoring)
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
// but marked accordingly.
func ScoreHosts(db *sql.DB, constraints Constraints) ([]Score, error) {
	hosts, err := inventory.LoadEmbeddedHosts()
	if err != nil {
		return nil, fmt.Errorf("load inventory: %w", err)
	}

	var scores []Score
	for _, h := range hosts {
		score := scoreHost(db, h, constraints)
		scores = append(scores, score)
	}

	// Sort: eligible first, then by score descending
	sortScores(scores)
	return scores, nil
}

// BestHost returns the best eligible host, or an error if none qualify.
func BestHost(db *sql.DB, constraints Constraints) (string, []string, error) {
	scores, err := ScoreHosts(db, constraints)
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

func scoreHost(db *sql.DB, host inventory.HostSpec, c Constraints) Score {
	s := Score{Host: host.Name, Eligible: true}

	// Hard constraint: GPU class
	if c.GPUClass != "" {
		found := false
		for _, gpu := range host.GPUs {
			if strings.EqualFold(gpu.Class, c.GPUClass) {
				found = true
				break
			}
		}
		if !found {
			s.Eligible = false
			s.Reasons = append(s.Reasons, fmt.Sprintf("no %s GPU", c.GPUClass))
			return s
		}
		s.Total += 10
		s.Reasons = append(s.Reasons, fmt.Sprintf("has %s GPU", c.GPUClass))
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
	}

	// Soft factor: data locality
	if db != nil && len(c.Inputs) > 0 {
		localCount := 0
		for _, ref := range c.Inputs {
			asset, ok := dataloc.ParseAssetRef(ref)
			if !ok {
				continue
			}
			entries, err := dataloc.FindAssetHosts(db, asset)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.Host == host.Name {
					localCount++
					break
				}
			}
		}
		if localCount > 0 {
			localityScore := float64(localCount) / float64(len(c.Inputs)) * 5.0
			s.Total += localityScore
			s.Reasons = append(s.Reasons, fmt.Sprintf("%d/%d inputs local", localCount, len(c.Inputs)))
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
