// Package campaign provides grouping and lifecycle management for cloud GPU campaigns.
package campaign

import (
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/workdir"
)

// InstanceGroup represents a group of jobs that share compatible GPU requirements
// and can run sequentially on a single cloud instance.
type InstanceGroup struct {
	GPUClass string // Normalized GPU class (uppercase), e.g. "H100"
	GPUMemGB int    // Supremum of GPU memory across all jobs in the group
	DiskGB   int    // Estimated disk space needed (0 = use default)
	Image    string // Per-project Docker image override ("" = use global default)
	Jobs     []*db.Job
}

// GroupByGPUSupremum groups unplaced jobs by compatible GPU class, using the
// maximum memory requirement across the group as the supremum. Jobs with
// compatible GPU constraints (e.g., "" and "nvidia", or "nvidia" and "ampere")
// are merged into the same group using the most specific constraint.
//
// Returns groups sorted by descending GPU memory.
func GroupByGPUSupremum(jobs []*db.Job) []InstanceGroup {
	var groups []InstanceGroup

	for _, job := range jobs {
		if job == nil || job.EffectiveStatus() != db.StatusQueued || job.HasAssignedHost() {
			continue
		}

		mem := 0
		if job.GPUMemGB != nil {
			mem = *job.GPUMemGB
		}

		merged := false
		for i := range groups {
			supremum, ok := gpuClassSupremum(groups[i].GPUClass, job.GPUClass)
			if ok {
				groups[i].GPUClass = supremum
				groups[i].Jobs = append(groups[i].Jobs, job)
				if mem > groups[i].GPUMemGB {
					groups[i].GPUMemGB = mem
				}
				merged = true
				break
			}
		}

		if !merged {
			groups = append(groups, InstanceGroup{
				GPUClass: strings.ToUpper(job.GPUClass),
				GPUMemGB: mem,
				Jobs:     []*db.Job{job},
			})
		}
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].GPUMemGB != groups[j].GPUMemGB {
			return groups[i].GPUMemGB > groups[j].GPUMemGB
		}
		return groups[i].GPUClass < groups[j].GPUClass
	})

	return groups
}

// FilterByGPUClass returns only the groups whose GPUClass matches filter (case-insensitive).
// Returns all groups if filter is empty.
func FilterByGPUClass(groups []InstanceGroup, filter string) []InstanceGroup {
	if filter == "" {
		return groups
	}
	var filtered []InstanceGroup
	for _, g := range groups {
		if strings.EqualFold(g.GPUClass, filter) {
			filtered = append(filtered, g)
		}
	}
	return filtered
}

// SplitGroupsByImage further subdivides instance groups so that all jobs in a
// group use a compatible Docker image. Each job's image is resolved from its
// project config (.weft.toml [cloud] image). Compatible CUDA images with the
// same version and OS but different variants (base/runtime/devel) are merged
// using the most capable variant.
func SplitGroupsByImage(groups []InstanceGroup) []InstanceGroup {
	type imageGroup struct {
		image string
		jobs  []*db.Job
	}

	// Pre-compute the auto-selected PyTorch image (constant across all jobs).
	defaultCUDA, _, _ := parseCUDAImage(cloud.DefaultImage)
	autoTorchImage := torchImageForCUDAVersion(defaultCUDA)

	var result []InstanceGroup
	for _, g := range groups {
		var subs []imageGroup

		for _, job := range g.Jobs {
			localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
			img := config.ProjectCloudImage(localDir)

			hasTorch := localDir != "" && hasCUDAPackages([]string{localDir})

			// Auto-select a PyTorch image when the project depends on torch
			// but no explicit image is configured. This avoids a ~5min cold
			// uv sync of torch + CUDA wheels on every instance launch.
			if img == "" && hasTorch && autoTorchImage != "" {
				img = autoTorchImage
			}

			// Warn when an explicit image doesn't include PyTorch but the
			// project has torch dependencies.
			if img != "" && hasTorch && !isPyTorchImage(img) {
				log.Printf("warning: job %d has torch dependencies but image %q does not include PyTorch — consider a pytorch/pytorch image", job.ID, img)
			}

			merged := false
			for i := range subs {
				supremum, ok := imageSupremum(subs[i].image, img)
				if ok {
					subs[i].image = supremum
					subs[i].jobs = append(subs[i].jobs, job)
					merged = true
					break
				}
			}
			if !merged {
				subs = append(subs, imageGroup{image: img, jobs: []*db.Job{job}})
			}
		}

		for _, sub := range subs {
			result = append(result, InstanceGroup{
				GPUClass: g.GPUClass,
				GPUMemGB: g.GPUMemGB,
				DiskGB:   g.DiskGB,
				Image:    sub.image,
				Jobs:     sub.jobs,
			})
		}
	}
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

// HasComputeIntensiveJob returns true if any job in the group has the compute-intensive tag.
func (g InstanceGroup) HasComputeIntensiveJob() bool {
	for _, job := range g.Jobs {
		if job.HasTag(db.TagComputeIntensive) {
			return true
		}
	}
	return false
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
