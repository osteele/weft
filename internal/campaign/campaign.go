// Package campaign provides grouping and lifecycle management for cloud GPU campaigns.
package campaign

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/placement"
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

// ModelSizeFunc returns the cached size in bytes for a model ID (without the
// "hf:" prefix). It must not make network calls. Returns (0, false) if unknown.
type ModelSizeFunc func(modelID string) (sizeBytes int64, ok bool)

// GroupByGPUSupremum groups unplaced jobs by compatible GPU class, using the
// maximum memory requirement across the group as the supremum. It delegates
// to [GroupByAffinity] with no size function, so unconstrained jobs are grouped
// by ref-count overlap only.
//
// Returns groups sorted by descending GPU memory.
func GroupByGPUSupremum(jobs []*db.Job) []InstanceGroup {
	return GroupByAffinity(jobs, nil)
}

// GroupByAffinity groups unplaced jobs in three buckets:
//
//  1. Pinned jobs (exact GPU model like "3090", "h100") are merged by GPU
//     compatibility, identical to the legacy [GroupByGPUSupremum] behavior.
//  2. Floatable jobs (family/generation like "nvidia", "ampere", "ampere+")
//     and unconstrained jobs (empty GPUClass) are grouped by shared data
//     affinity. Jobs sharing HF model inputs are co-located to amortize
//     downloads. Floatable constraints are preserved on the group for offer
//     filtering but allow the strategy engine to select the best GPU.
//     If sizeFunc is nil, affinity is based on ref-count overlap only.
//
// Returns groups sorted by descending GPU memory.
func GroupByAffinity(jobs []*db.Job, sizeFunc ModelSizeFunc) []InstanceGroup {
	var pinned, floatable []*db.Job
	for _, job := range jobs {
		if job == nil || job.EffectiveStatus() != db.StatusQueued || job.HasAssignedHost() {
			continue
		}
		gpuClass := strings.TrimSpace(job.GPUClass)
		if gpuClass != "" && !placement.ParseGPUConstraint(gpuClass).IsFloatable() {
			pinned = append(pinned, job)
		} else {
			floatable = append(floatable, job)
		}
	}

	groups := groupConstrained(pinned)
	affinityGroups := affinityGroupUnconstrained(floatable, sizeFunc)
	groups = append(groups, affinityGroups...)

	sortGroups(groups)
	return groups
}

// groupConstrained merges constrained jobs by GPU class compatibility.
func groupConstrained(jobs []*db.Job) []InstanceGroup {
	var groups []InstanceGroup
	for _, job := range jobs {
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
	return groups
}

// affinityGroupUnconstrained groups unconstrained and floatable jobs by shared
// HF model inputs using a greedy algorithm. Jobs are processed in order of
// descending total input size (so large-model jobs anchor groups), falling back
// to job ID for determinism. When merging, GPU constraints must be compatible
// (via [gpuClassSupremum]); the group's GPUClass is set to the narrowest
// compatible constraint so offer filtering still applies.
func affinityGroupUnconstrained(jobs []*db.Job, sizeFunc ModelSizeFunc) []InstanceGroup {
	if len(jobs) == 0 {
		return nil
	}

	// Pre-compute per-job HF input sets and total sizes for sorting.
	type jobInfo struct {
		job       *db.Job
		hfInputs  map[string]struct{}
		totalSize int64
	}
	infos := make([]jobInfo, 0, len(jobs))
	for _, job := range jobs {
		hfInputs := jobHFModelIDs(job)
		var totalSize int64
		if sizeFunc != nil {
			for id := range hfInputs {
				if sz, ok := sizeFunc(id); ok {
					totalSize += sz
				}
			}
		}
		infos = append(infos, jobInfo{job: job, hfInputs: hfInputs, totalSize: totalSize})
	}

	// Sort by descending total input size, then by job ID for determinism.
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].totalSize != infos[j].totalSize {
			return infos[i].totalSize > infos[j].totalSize
		}
		return infos[i].job.ID < infos[j].job.ID
	})

	type groupState struct {
		group   InstanceGroup
		hfUnion map[string]struct{} // union of HF model IDs across all jobs
	}
	var groups []groupState

	const refCountWeight = 10e9 // 10 GB — fallback weight per shared ref when size unknown

	for _, info := range infos {
		mem := 0
		if info.job.GPUMemGB != nil {
			mem = *info.job.GPUMemGB
		}

		jobGPU := strings.TrimSpace(info.job.GPUClass)

		bestIdx := -1
		var bestScore float64
		for i, g := range groups {
			// Skip groups with incompatible GPU constraints (e.g. ampere vs hopper).
			if _, gpuOK := gpuClassSupremum(g.group.GPUClass, jobGPU); !gpuOK {
				continue
			}
			score := sharedInputScore(g.hfUnion, info.hfInputs, sizeFunc, refCountWeight)
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
		}

		if bestIdx >= 0 && bestScore > 0 {
			g := &groups[bestIdx]
			g.group.Jobs = append(g.group.Jobs, info.job)
			supremum, _ := gpuClassSupremum(g.group.GPUClass, jobGPU)
			g.group.GPUClass = supremum
			if mem > g.group.GPUMemGB {
				g.group.GPUMemGB = mem
			}
			for id := range info.hfInputs {
				g.hfUnion[id] = struct{}{}
			}
		} else {
			hfUnion := make(map[string]struct{}, len(info.hfInputs))
			for id := range info.hfInputs {
				hfUnion[id] = struct{}{}
			}
			groups = append(groups, groupState{
				group: InstanceGroup{
					GPUClass: strings.ToUpper(jobGPU),
					GPUMemGB: mem,
					Jobs:     []*db.Job{info.job},
				},
				hfUnion: hfUnion,
			})
		}
	}

	result := make([]InstanceGroup, len(groups))
	for i, g := range groups {
		result[i] = g.group
	}
	return result
}

// jobHFModelIDs extracts the HF model IDs (without "hf:" prefix) from a job's
// declared and observed inputs.
func jobHFModelIDs(job *db.Job) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, ref := range job.Inputs {
		if asset, ok := dataloc.ParseAssetRef(ref); ok && asset.Kind == dataloc.AssetHFModel {
			ids[asset.ID] = struct{}{}
		}
	}
	for _, ref := range job.ObservedInputs {
		if asset, ok := dataloc.ParseAssetRef(ref); ok && asset.Kind == dataloc.AssetHFModel {
			ids[asset.ID] = struct{}{}
		}
	}
	return ids
}

// sharedInputScore computes an affinity score between a group's HF model set
// and a job's HF model set. When sizeFunc provides sizes, the score is the sum
// of shared model sizes plus a per-ref fallback for unsized models. When
// sizeFunc is nil, the score is purely count-based.
func sharedInputScore(groupIDs, jobIDs map[string]struct{}, sizeFunc ModelSizeFunc, refWeight float64) float64 {
	var score float64
	for id := range jobIDs {
		if _, shared := groupIDs[id]; !shared {
			continue
		}
		if sizeFunc != nil {
			if sz, ok := sizeFunc(id); ok && sz > 0 {
				score += float64(sz)
				continue
			}
		}
		score += refWeight
	}
	return score
}

func sortGroups(groups []InstanceGroup) {
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].GPUMemGB != groups[j].GPUMemGB {
			return groups[i].GPUMemGB > groups[j].GPUMemGB
		}
		return groups[i].GPUClass < groups[j].GPUClass
	})
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
				slog.Warn("job has torch dependencies but image does not include PyTorch", "component", "campaign", "job_id", job.ID, "image", img)
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
