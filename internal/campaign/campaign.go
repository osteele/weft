// Package campaign provides grouping and lifecycle management for cloud GPU campaigns.
package campaign

import (
	"sort"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/workdir"
)

// InstanceGroup represents a group of jobs that share compatible GPU requirements
// and can run sequentially on a single cloud instance.
type InstanceGroup struct {
	GPUClass string // Normalized GPU class (uppercase), e.g. "H100"
	GPUMemGB int    // Supremum of GPU memory across all jobs in the group
	DiskGB   int    // Estimated disk space needed (0 = use default)
	Jobs     []*db.Job
}

// GroupByGPUSupremum groups needs_rental jobs by GPU class, using the maximum
// memory requirement across the group as the supremum. Jobs with the same
// GPUClass (case-insensitive) are placed in the same group. Jobs with no class
// are grouped by memory tier alone.
//
// Returns groups sorted by descending GPU memory.
func GroupByGPUSupremum(jobs []*db.Job) []InstanceGroup {
	type groupKey struct {
		class  string
		hasGPU bool // distinguishes "" class with mem vs "" class without mem
	}

	groups := make(map[groupKey]*InstanceGroup)

	for _, job := range jobs {
		if job.Status != db.StatusNeedsRental {
			continue
		}

		class := strings.ToUpper(job.GPUClass)
		mem := 0
		if job.GPUMemGB != nil {
			mem = *job.GPUMemGB
		}

		key := groupKey{class: class, hasGPU: class != "" || mem > 0}
		g, ok := groups[key]
		if !ok {
			g = &InstanceGroup{GPUClass: class}
			groups[key] = g
		}
		g.Jobs = append(g.Jobs, job)
		if mem > g.GPUMemGB {
			g.GPUMemGB = mem
		}
	}

	result := make([]InstanceGroup, 0, len(groups))
	for _, g := range groups {
		result = append(result, *g)
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].GPUMemGB != result[j].GPUMemGB {
			return result[i].GPUMemGB > result[j].GPUMemGB
		}
		return result[i].GPUClass < result[j].GPUClass
	})

	return result
}

// SourceDirs returns the unique local absolute paths for all jobs in the group.
// Each path is resolved from the job's working directory to a local absolute path.
// Paths that cannot be resolved (relative, unrecognized prefix) are skipped.
func (g InstanceGroup) SourceDirs() []string {
	seen := make(map[string]bool)
	var dirs []string
	for _, job := range g.Jobs {
		d := workdir.ResolveLocal(job.EffectiveWorkingDir())
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// AllInputs returns the deduplicated input refs from all jobs in the group.
func (g InstanceGroup) AllInputs() []string {
	seen := make(map[string]struct{})
	var inputs []string
	for _, job := range g.Jobs {
		for _, input := range job.Inputs {
			if _, ok := seen[input]; !ok {
				seen[input] = struct{}{}
				inputs = append(inputs, input)
			}
		}
	}
	return inputs
}

// GPUSpec returns a human-readable GPU spec string for the group.
func (g InstanceGroup) GPUSpec() string {
	switch {
	case g.GPUClass != "" && g.GPUMemGB > 0:
		return g.GPUClass + " ≥" + strconv.Itoa(g.GPUMemGB) + "GB"
	case g.GPUClass != "":
		return g.GPUClass
	case g.GPUMemGB > 0:
		return "≥" + strconv.Itoa(g.GPUMemGB) + "GB"
	default:
		return "GPU"
	}
}
