// Package campaign provides grouping and lifecycle management for cloud GPU campaigns.
package campaign

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/imagereq"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/workdir"
)

const sglangRuntimeImage = "ghcr.io/osteele/sglang-runtime:v0.5.10.post1"

// vramTiers lists standard GPU VRAM sizes in GB, used to determine whether two
// jobs' memory requirements are in the same tier for grouping purposes.
var vramTiers = []int{12, 16, 24, 48, 80, 141}

const refCountWeight = 10e9 // 10 GB — fallback weight per shared ref when size unknown

// vramTierOf returns the VRAM tier for a given memory requirement in GB.
// Jobs in the same tier can share an instance without waste.
func vramTierOf(memGB int) int {
	for _, tier := range vramTiers {
		if memGB <= tier {
			return tier
		}
	}
	return memGB // above all tiers
}

// InstanceGroup represents a group of jobs that share compatible GPU requirements
// and can run sequentially on a single cloud instance.
type InstanceGroup struct {
	GPUClass         string   // Normalized GPU class (uppercase), e.g. "H100"
	Provider         string   // Requested provider ("vastai" or "runpod"), empty = any
	GPUMemGB         int      // Supremum of GPU memory across all jobs in the group
	MaxGPUMemGB      int      // Legacy metadata retained for old rows; not used for placement
	DiskGB           int      // Estimated disk space needed (0 = use default)
	Image            string   // Docker image override/default for this group ("" = use global default)
	MinDriverVersion int      // Minimum NVIDIA driver version required by the image
	MinCUDAVersion   string   // Minimum provider CUDA compatibility required by the image
	ImagePullSecret  string   // Registry config key for private image pulls
	VastCapAdd       []string // Vast.ai-only --cap-add values (nil = none)
	Preemptible      bool     // all jobs in the group allow interruptible placement
	// MaxComputeCap is the most restrictive (lowest) CUDA compute-capability
	// upper bound across the jobs in the group, expressed as a string like
	// "9.0" or "12.0". Empty string means "no upper bound" (at least one job
	// in the group has no inferred limit). Inferred from each job's torch
	// pin or set explicitly via [tool.weft] gpu-arch-max.
	MaxComputeCap string
	// MinComputeCap is the most restrictive (highest) CUDA compute-capability
	// lower bound across the jobs in the group. Empty string means "no lower
	// bound".
	MinComputeCap string
	Jobs          []*db.Job
}

// ApplyImageMetadataRequirements augments groups with constraints declared by
// their Docker image config. Explicit project/script requirements already on
// the group are preserved and win when stricter.
func ApplyImageMetadataRequirements(cfg *config.Config, groups []InstanceGroup) []InstanceGroup {
	if cfg == nil {
		return groups
	}
	for i := range groups {
		image := strings.TrimSpace(groups[i].Image)
		if image == "" || !shouldFetchImageRequirements(image) {
			continue
		}
		auth, err := cfg.RegistryAuthForImage(image, groups[i].ImagePullSecret)
		if err != nil {
			slog.Warn("image registry auth unavailable", "component", "campaign", "image", image, "error", err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		req, err := imagereq.Resolve(ctx, image, auth)
		cancel()
		if err != nil {
			slog.Warn("image requirement resolution failed", "component", "campaign", "image", image, "error", err)
			continue
		}
		groups[i].MinDriverVersion = maxInt(groups[i].MinDriverVersion, req.MinDriverVersion)
		groups[i].MinCUDAVersion = maxCUDAVersionString(groups[i].MinCUDAVersion, req.MinCUDAVersion)
	}
	// Back-fill driver floors from CUDA floors for every group, not just the
	// label-fetched ones — `shouldFetchImageRequirements` skips the very
	// auto-selected pytorch/* and nvidia/cuda images that motivate the
	// back-fill, so doing it inside the label-fetch branch would be dead code
	// for the primary use case.
	for i := range groups {
		merged := imagereq.BackfillDriverFromCUDA(cloud.ImageRequirements{
			MinCUDAVersion:   groups[i].MinCUDAVersion,
			MinDriverVersion: groups[i].MinDriverVersion,
		})
		groups[i].MinDriverVersion = merged.MinDriverVersion
	}
	return groups
}

func shouldFetchImageRequirements(image string) bool {
	repo := imageRepo(image)
	if repo == "nvidia/cuda" || repo == "pytorch/pytorch" || strings.HasPrefix(repo, "runpod/") {
		return false
	}
	return strings.TrimSpace(image) != ""
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

	sortJobsWithinGroups(groups)
	sortGroups(groups)
	return groups
}

// groupConstrained merges constrained jobs by GPU class compatibility.
func groupConstrained(jobs []*db.Job) []InstanceGroup {
	var groups []InstanceGroup
	for _, job := range jobs {
		provider, _ := db.RequestedProvider(job.Tags)
		mem := 0
		if job.GPUMemGB != nil {
			mem = *job.GPUMemGB
		}

		merged := false
		for i := range groups {
			if groups[i].Preemptible != job.UsesPreemptiblePlacement() {
				continue
			}
			if !strings.EqualFold(groups[i].Provider, provider) {
				continue
			}
			if vramTierOf(groups[i].GPUMemGB) != vramTierOf(mem) {
				continue
			}
			supremum, ok := gpuClassSupremum(groups[i].GPUClass, job.GPUClass)
			if !ok {
				continue
			}
			groups[i].GPUClass = supremum
			groups[i].Jobs = append(groups[i].Jobs, job)
			if mem > groups[i].GPUMemGB {
				groups[i].GPUMemGB = mem
			}
			merged = true
			break
		}

		if !merged {
			groups = append(groups, InstanceGroup{
				GPUClass:    strings.ToUpper(job.GPUClass),
				Provider:    provider,
				GPUMemGB:    mem,
				Preemptible: job.UsesPreemptiblePlacement(),
				Jobs:        []*db.Job{job},
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

	for _, info := range infos {
		mem := 0
		if info.job.GPUMemGB != nil {
			mem = *info.job.GPUMemGB
		}

		jobGPU := strings.TrimSpace(info.job.GPUClass)
		jobPreemptible := info.job.UsesPreemptiblePlacement()
		jobProvider, _ := db.RequestedProvider(info.job.Tags)

		bestIdx := -1
		var bestScore float64
		jobTier := vramTierOf(mem)
		for i, g := range groups {
			if g.group.Preemptible != jobPreemptible {
				continue
			}
			if !strings.EqualFold(g.group.Provider, jobProvider) {
				continue
			}
			// Skip groups with incompatible GPU constraints (e.g. ampere vs hopper).
			if _, gpuOK := gpuClassSupremum(g.group.GPUClass, jobGPU); !gpuOK {
				continue
			}
			// Skip groups in a different VRAM tier to avoid over-provisioning.
			// An 8GB job grouped with a 20GB job forces the whole group to ≥20GB,
			// routing cheap jobs to expensive GPUs.
			if vramTierOf(g.group.GPUMemGB) != jobTier {
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
					GPUClass:    strings.ToUpper(jobGPU),
					Provider:    jobProvider,
					GPUMemGB:    mem,
					Preemptible: jobPreemptible,
					Jobs:        []*db.Job{info.job},
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

func assetOverlapSavedHours(groups []InstanceGroup) float64 {
	overlapBytes := assetOverlapBytes(groups)
	if overlapBytes <= 0 {
		return 0
	}
	return overlapBytes / estimatedLANCopyRate / 3600
}

func assetOverlapBytes(groups []InstanceGroup) float64 {
	var total float64
	for _, group := range groups {
		jobAssets := make([]map[dataloc.DataAsset]struct{}, 0, len(group.Jobs))
		for _, job := range group.Jobs {
			jobAssets = append(jobAssets, jobInputAssets(job))
		}
		for i := 0; i < len(jobAssets); i++ {
			for j := i + 1; j < len(jobAssets); j++ {
				total += pairwiseAssetOverlapBytes(jobAssets[i], jobAssets[j])
			}
		}
	}
	return total
}

func pairwiseAssetOverlapBytes(a, b map[dataloc.DataAsset]struct{}) float64 {
	var total float64
	for asset := range a {
		if _, ok := b[asset]; !ok {
			continue
		}
		total += assetOverlapWeight(asset)
	}
	return total
}

func assetOverlapWeight(asset dataloc.DataAsset) float64 {
	if asset.Kind == dataloc.AssetHFModel {
		if sz, ok := dataloc.LookupCachedModelSize(asset.ID); ok && sz > 0 {
			return float64(sz)
		}
	}
	return refCountWeight
}

func jobInputAssets(job *db.Job) map[dataloc.DataAsset]struct{} {
	assets := make(map[dataloc.DataAsset]struct{})
	if job == nil {
		return assets
	}
	for _, ref := range job.Inputs {
		if asset, ok := dataloc.ParseAssetRef(ref); ok && isOverlapAsset(asset) {
			assets[asset] = struct{}{}
		}
	}
	for _, ref := range job.ObservedInputs {
		if asset, ok := dataloc.ParseAssetRef(ref); ok && isOverlapAsset(asset) {
			assets[asset] = struct{}{}
		}
	}
	return assets
}

func isOverlapAsset(asset dataloc.DataAsset) bool {
	return asset.Kind == dataloc.AssetHFModel || asset.Kind == dataloc.AssetHFDataset
}

func sortGroups(groups []InstanceGroup) {
	sort.Slice(groups, func(i, j int) bool {
		aPriority := maxGroupPriority(groups[i])
		bPriority := maxGroupPriority(groups[j])
		if aPriority != bPriority {
			return aPriority > bPriority
		}
		if groups[i].GPUMemGB != groups[j].GPUMemGB {
			return groups[i].GPUMemGB > groups[j].GPUMemGB
		}
		if groups[i].Provider != groups[j].Provider {
			return groups[i].Provider < groups[j].Provider
		}
		return groups[i].GPUClass < groups[j].GPUClass
	})
}

func sortJobsWithinGroups(groups []InstanceGroup) {
	for i := range groups {
		sort.SliceStable(groups[i].Jobs, func(a, b int) bool {
			return db.SchedulingLess(groups[i].Jobs[a], groups[i].Jobs[b])
		})
	}
}

func maxGroupPriority(group InstanceGroup) int {
	maxPriority := 0
	for _, job := range group.Jobs {
		if job != nil && job.Priority > maxPriority {
			maxPriority = job.Priority
		}
	}
	return maxPriority
}

func mergeMaxComputeCap(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	if placement.CompareComputeCap(a, b) <= 0 {
		return a
	}
	return b
}

func mergeMinComputeCap(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if placement.CompareComputeCap(a, b) >= 0 {
		return a
	}
	return b
}

// MergeCompatibleGroups combines instance groups that have compatible GPU
// constraints and Docker images into fewer, larger groups. This amortizes
// instance launch overhead and rental minimums when jobs have no data affinity.
// Groups are merged greedily: each group joins the first compatible group found.
func MergeCompatibleGroups(groups []InstanceGroup) []InstanceGroup {
	if len(groups) <= 1 {
		return groups
	}

	var merged []InstanceGroup
	for _, g := range groups {
		found := false
		for i := range merged {
			gpuSup, gpuOK := gpuClassSupremum(merged[i].GPUClass, g.GPUClass)
			if !gpuOK {
				continue
			}
			if merged[i].Preemptible != g.Preemptible {
				continue
			}
			if !strings.EqualFold(merged[i].Provider, g.Provider) {
				continue
			}
			if vramTierOf(merged[i].GPUMemGB) != vramTierOf(g.GPUMemGB) {
				continue
			}
			imgSup, imgOK := imageSupremum(merged[i].Image, g.Image)
			if !imgOK {
				continue
			}
			merged[i].GPUClass = gpuSup
			merged[i].Image = imgSup
			merged[i].Jobs = append(merged[i].Jobs, g.Jobs...)
			if g.GPUMemGB > merged[i].GPUMemGB {
				merged[i].GPUMemGB = g.GPUMemGB
			}
			if g.DiskGB > merged[i].DiskGB {
				merged[i].DiskGB = g.DiskGB
			}
			merged[i].MaxComputeCap = mergeMaxComputeCap(merged[i].MaxComputeCap, g.MaxComputeCap)
			merged[i].MinComputeCap = mergeMinComputeCap(merged[i].MinComputeCap, g.MinComputeCap)
			// Preserve hard CUDA/driver floors across the merge — they're
			// later read by offerConstraintsForGroup to filter Vast offers,
			// and dropping them would defeat the back-fill set up in
			// SplitGroupsByImage / ResolveJobImageSettings.
			merged[i].MinDriverVersion = maxInt(merged[i].MinDriverVersion, g.MinDriverVersion)
			merged[i].MinCUDAVersion = maxCUDAVersionString(merged[i].MinCUDAVersion, g.MinCUDAVersion)
			found = true
			break
		}
		if !found {
			// Copy the group to avoid mutating the original
			merged = append(merged, InstanceGroup{
				GPUClass:         g.GPUClass,
				Provider:         g.Provider,
				GPUMemGB:         g.GPUMemGB,
				DiskGB:           g.DiskGB,
				Image:            g.Image,
				Preemptible:      g.Preemptible,
				MaxComputeCap:    g.MaxComputeCap,
				MinComputeCap:    g.MinComputeCap,
				MinDriverVersion: g.MinDriverVersion,
				MinCUDAVersion:   g.MinCUDAVersion,
				Jobs:             append([]*db.Job(nil), g.Jobs...),
			})
		}
	}

	sortGroups(merged)
	return merged
}

// SplitToParallel expands multi-job groups into one-job-per-group, using each
// job's individual GPU memory requirements instead of the group supremum. This
// allows the scoring function to evaluate running jobs in parallel across
// separate instances, each sized to the individual job's needs.
func SplitToParallel(groups []InstanceGroup) []InstanceGroup {
	var result []InstanceGroup
	for _, g := range groups {
		if len(g.Jobs) <= 1 {
			result = append(result, InstanceGroup{
				GPUClass:         g.GPUClass,
				Provider:         g.Provider,
				GPUMemGB:         g.GPUMemGB,
				DiskGB:           g.DiskGB,
				Image:            g.Image,
				Preemptible:      g.Preemptible,
				MaxComputeCap:    g.MaxComputeCap,
				MinComputeCap:    g.MinComputeCap,
				MinDriverVersion: g.MinDriverVersion,
				MinCUDAVersion:   g.MinCUDAVersion,
				Jobs:             append([]*db.Job(nil), g.Jobs...),
			})
			continue
		}
		for _, job := range g.Jobs {
			mem := 0
			if job.GPUMemGB != nil {
				mem = *job.GPUMemGB
			}
			result = append(result, InstanceGroup{
				GPUClass:         g.GPUClass,
				Provider:         g.Provider,
				GPUMemGB:         mem,
				DiskGB:           g.DiskGB,
				Image:            g.Image,
				Preemptible:      g.Preemptible,
				MaxComputeCap:    groupMaxComputeCap(nil, []*db.Job{job}),
				MinComputeCap:    groupMinComputeCap([]*db.Job{job}),
				MinDriverVersion: g.MinDriverVersion,
				MinCUDAVersion:   g.MinCUDAVersion,
				Jobs:             []*db.Job{job},
			})
		}
	}
	sortGroups(result)
	return result
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
// group use a compatible Docker image. Each job's image is resolved from script
// metadata and project config. Compatible CUDA images with the same version and
// OS but different variants (base/runtime/devel) are merged using the most
// capable variant.
//
// `database` is used to lazily backfill jobs.max_compute_cap rows that are
// NULL (legacy rows or post-refresh) so the resulting group's MaxComputeCap
// reflects the current torch pin / script meta. Pass nil only in tests where
// every job already has a non-NULL cap.
func SplitGroupsByImage(database *sql.DB, groups []InstanceGroup) []InstanceGroup {
	type imageGroup struct {
		image            string
		minDriverVersion int
		minCUDAVersion   string
		imagePullSecret  string
		vastCapAdd       []string
		jobs             []*db.Job
	}

	// Fallback PyTorch image for torch projects whose pinned CUDA cannot be
	// resolved; pin-aware selection in the loop below is preferred.
	defaultCUDA, _, _ := parseCUDAImage(cloud.DefaultImage)
	fallbackTorchImage := torchImageForCUDAVersion(defaultCUDA)

	var result []InstanceGroup
	for _, g := range groups {
		var subs []imageGroup

		for _, job := range g.Jobs {
			localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
			img, req, imagePullSecret := ResolveJobImageSettings(localDir, job.Command)
			// Apply per-job CLI overrides (e.g. --cuda-driver-min) on top of
			// project / script / lockfile-derived requirements. The CLI flag
			// is the only place where the operator can express a constraint
			// the lockfile and PEP 723 can't see — e.g. an isolated venv
			// built on the rental with a known torch CUDA tag.
			if job.CLIResourceOverrides != nil && job.CLIResourceOverrides.MinCUDAVersion != "" {
				req = imagereq.Merge(req, cloud.ImageRequirements{MinCUDAVersion: job.CLIResourceOverrides.MinCUDAVersion})
				req = imagereq.BackfillDriverFromCUDA(req)
			}
			explicitImage := img != ""
			vastCapAdd := ResolveJobVastCapAdd(localDir, job.Command)

			hasTorch := localDir != "" && hasCUDAPackages([]string{localDir})

			// Resolve the project's pinned torch CUDA (e.g. "12.4"). When set,
			// it governs the image's CUDA: a pinned torch wheel cannot use a
			// different CUDA runtime, so the GPU-constraint upgrade below is
			// skipped for pinned projects.
			pinCUDA := ""
			if hasTorch {
				if pin := dataloc.ScanTorchPin(localDir); pin != nil {
					pinCUDA = dataloc.CUDAVariantVersion(pin.CudaVariant)
				}
			}

			// Auto-select a PyTorch image when the project depends on torch
			// but no explicit image is configured. This avoids a ~5min cold
			// uv sync of torch + CUDA wheels on every instance launch. The
			// image's CUDA must match the project's torch pin; fall back to the
			// default image when the pin's CUDA cannot be resolved.
			if img == "" && hasTorch {
				if pinned := torchImageForCUDAVersion(pinCUDA); pinned != "" {
					img = pinned
				} else if fallbackTorchImage != "" {
					img = fallbackTorchImage
				}
			}
			if img == "" && jobUsesFramework(localDir, job.Command, "sglang") {
				img = sglangRuntimeImage
			}

			// Auto-upgrade CUDA images when the GPU constraint requires a newer
			// toolkit (e.g., Blackwell needs CUDA >= 12.8). Skipped when the
			// project pins a torch CUDA: the pinned wheel cannot use a newer
			// runtime, and arch-max filtering already keeps such jobs off GPUs
			// that would need one. Explicit non-CUDA images are left unchanged
			// and will fail later with a clear compatibility reason.
			requiredCUDA := placement.MinCUDAForConstraint(g.GPUClass)
			if requiredCUDA > 0 && pinCUDA == "" {
				effectiveImage := img
				if effectiveImage == "" {
					effectiveImage = cloud.DefaultImage
				}
				if imageCUDA, _, ok := parseCUDAImage(effectiveImage); ok {
					currentCUDA := parseCUDAVersionFloat(imageCUDA)
					if currentCUDA > 0 && currentCUDA < requiredCUDA {
						preferTorch := hasTorch || isPyTorchImage(effectiveImage)
						if upgraded := chooseAutoImageForMinCUDA(requiredCUDA, preferTorch); upgraded != "" && upgraded != effectiveImage {
							img = upgraded
							slog.Debug("auto-selected CUDA-compatible image for GPU constraint",
								"component", "campaign",
								"gpu_class", g.GPUClass,
								"required_cuda", requiredCUDA,
								"previous_image", effectiveImage,
								"selected_image", upgraded,
								"explicit_image", explicitImage)
						}
					}
				}
			}

			// Warn when an explicit image doesn't include PyTorch but the
			// project has torch dependencies.
			if img != "" && hasTorch && !isPyTorchImage(img) {
				slog.Warn("job has torch dependencies but image does not include PyTorch", "component", "campaign", "job_id", job.ID, "image", img)
			}

			// RunPod SSH bootstrap requires runpod/* images with init/sshd.
			// The remap is automatic and benign — log at debug since it
			// fires every pass for every torch-based job heading to runpod
			// and produces no actionable signal.
			if strings.EqualFold(g.Provider, string(cloud.ProviderRunpod)) {
				originalImage := img
				img = normalizeRunpodGroupImage(img)
				if originalImage != "" && originalImage != img {
					slog.Debug("remapped image to RunPod-compatible image",
						"component", "campaign",
						"job_id", job.ID,
						"provider", g.Provider,
						"original_image", originalImage,
						"selected_image", img)
				}
			}

			merged := false
			for i := range subs {
				supremum, ok := imageSupremum(subs[i].image, img)
				if ok {
					subs[i].image = supremum
					subs[i].minDriverVersion = maxInt(subs[i].minDriverVersion, req.MinDriverVersion)
					subs[i].minCUDAVersion = maxCUDAVersionString(subs[i].minCUDAVersion, req.MinCUDAVersion)
					if subs[i].imagePullSecret == "" {
						subs[i].imagePullSecret = imagePullSecret
					}
					subs[i].vastCapAdd = mergeVastCapAdd(subs[i].vastCapAdd, vastCapAdd)
					subs[i].jobs = append(subs[i].jobs, job)
					merged = true
					break
				}
			}
			if !merged {
				subs = append(subs, imageGroup{
					image:            img,
					minDriverVersion: req.MinDriverVersion,
					minCUDAVersion:   req.MinCUDAVersion,
					imagePullSecret:  imagePullSecret,
					vastCapAdd:       vastCapAdd,
					jobs:             []*db.Job{job},
				})
			}
		}

		for _, sub := range subs {
			result = append(result, InstanceGroup{
				GPUClass:         g.GPUClass,
				Provider:         g.Provider,
				GPUMemGB:         g.GPUMemGB,
				DiskGB:           g.DiskGB,
				Image:            sub.image,
				MinDriverVersion: sub.minDriverVersion,
				MinCUDAVersion:   sub.minCUDAVersion,
				ImagePullSecret:  sub.imagePullSecret,
				VastCapAdd:       sub.vastCapAdd,
				Preemptible:      g.Preemptible,
				MaxComputeCap:    groupMaxComputeCap(database, sub.jobs),
				MinComputeCap:    groupMinComputeCap(sub.jobs),
				Jobs:             sub.jobs,
			})
		}
	}
	return result
}

// ResolveJobImage returns the Docker image for a job from script metadata,
// per-script project defaults, or the project image. Returns empty string if no
// source specifies an image.
func ResolveJobImage(localDir, command string) string {
	img, _, _ := ResolveJobImageSettings(localDir, command)
	return img
}

// ResolveJobImageSettings returns the Docker image and explicit image
// requirements from project config and PEP 723 metadata.
func ResolveJobImageSettings(localDir, command string) (string, cloud.ImageRequirements, string) {
	var projectCloud config.ProjectCloudConfig
	var projectDir string
	if cfg, path, err := config.LoadProjectConfigWithPath(localDir); err != nil {
		slog.Warn("error loading project config for image resolution", "component", "campaign", "dir", localDir, "error", err)
	} else if cfg != nil {
		projectCloud = cfg.Cloud
		projectDir = filepath.Dir(path)
	}

	req, err := imagereq.Explicit(projectCloud.MinDriver, projectCloud.MinCUDA)
	if err != nil {
		slog.Warn("project cloud image requirement error", "component", "campaign", "error", err)
	}
	img := strings.TrimSpace(projectCloud.Image)
	if override := resolveProjectImageOverride(projectCloud.ImageOverrides, projectDir, localDir, command); override != "" {
		img = override
	}
	if img == "" && jobUsesFramework(localDir, command, "sglang") {
		img = sglangRuntimeImage
	}
	imagePullSecret := projectCloud.ImagePullSecret

	if meta, err := dataloc.ScanScriptMeta(localDir, command); err != nil {
		slog.Warn("script metadata error in image resolution", "component", "campaign", "error", err)
	} else if meta != nil {
		if meta.Image != "" {
			img = meta.Image
		}
		if scriptReq, reqErr := imagereq.Explicit(meta.MinDriver, meta.MinCUDA); reqErr != nil {
			slog.Warn("script image requirement error", "component", "campaign", "error", reqErr)
		} else {
			req = imagereq.Merge(req, scriptReq)
		}
		if imagePullSecret == "" {
			imagePullSecret = meta.ImagePullSecret
		}
	}
	// PEP 723 top-level `dependencies = [...]` is read independently of
	// [tool.weft]: a script may declare standard deps without configuring
	// weft, and we still want those deps to drive CUDA-floor inference.
	scriptDeps := dataloc.ScanScriptDependencies(localDir, command)
	// Cloud offers keep the torch pin's EXACT CUDA floor, deliberately
	// stricter than the on-prem family floor used by
	// placement.MinRuntimeFloorForJob. On-prem hosts are known machines
	// where minor-version compatibility (cu12x wheel on any R525+ driver)
	// is reliable; rental offers report cuda_max_good values that have
	// produced real cuda_driver_too_old failures when filtered only to the
	// family floor (see campaign_test.go's driver-floor regression).
	if torchCUDA := dataloc.TorchMinCUDAVersion(localDir); torchCUDA != "" {
		req = imagereq.Merge(req, cloud.ImageRequirements{MinCUDAVersion: torchCUDA})
		slog.Debug("auto-derived CUDA driver floor from torch pin",
			"component", "campaign", "cuda_floor", torchCUDA, "local_dir", localDir)
	}
	// Inline deps that the project's uv.lock doesn't see: `uv run --with X`
	// and PEP 723 `dependencies = [...]`. vLLM 0.17+ is the motivating case —
	// it bundles flash-attention 4, which needs CUDA 12.8 / driver 570
	// regardless of the torch wheel in the project lockfile (the lockfile
	// may pin torch+cu124, but `--with "vllm>=0.17"` resolves a fresh stack
	// on the rental that overrides it).
	deps := append([]dataloc.DepSpec{}, dataloc.ScanUVRunWith(command)...)
	deps = append(deps, dataloc.ParseDepSpecs(scriptDeps)...)
	if libCUDA := dataloc.LibraryMinCUDAFromDeps(deps); libCUDA != "" {
		req = imagereq.Merge(req, cloud.ImageRequirements{MinCUDAVersion: libCUDA})
		slog.Debug("auto-derived CUDA driver floor from inline library deps",
			"component", "campaign", "cuda_floor", libCUDA, "local_dir", localDir)
	}
	// Back-fill a driver floor from the CUDA floor when nothing else supplied
	// one. Auto-selected pytorch/nvidia base images bypass label fetching
	// (shouldFetchImageRequirements), so a project pinned at e.g. torch+cu128
	// would otherwise emit `cuda>=12.8` without the matching `driver>=570`,
	// and Vast can return offers whose advertised cuda_max_good clears the
	// CUDA gate while driver_version is too old for the wheel at runtime.
	req = imagereq.BackfillDriverFromCUDA(req)
	return img, req, imagePullSecret
}

func jobUsesFramework(localDir, command, framework string) bool {
	framework = strings.ToLower(framework)
	if strings.Contains(strings.ToLower(command), framework) {
		return true
	}
	if localDir != "" {
		if fileContains(filepath.Join(localDir, "pyproject.toml"), framework) {
			return true
		}
		for _, script := range dataloc.ExtractPythonScripts(command) {
			if !filepath.IsAbs(script) {
				script = filepath.Join(localDir, script)
			}
			if fileContains(script, framework) {
				return true
			}
		}
	}
	return false
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), needle)
}

func resolveProjectImageOverride(overrides map[string]string, projectDir, localDir, command string) string {
	if len(overrides) == 0 || projectDir == "" {
		return ""
	}
	scripts := dataloc.ExtractPythonScripts(command)
	if len(scripts) == 0 {
		return ""
	}

	script := scripts[0]
	if !filepath.IsAbs(script) {
		script = filepath.Join(localDir, script)
	}
	rel, err := filepath.Rel(projectDir, script)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return ""
	}
	rel = filepath.ToSlash(filepath.Clean(rel))

	var bestPattern, bestImage string
	for pattern, image := range overrides {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}
		normalizedPattern := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(pattern)), "./")
		matched, err := path.Match(normalizedPattern, rel)
		if err != nil {
			slog.Warn("invalid cloud image override pattern", "component", "campaign", "pattern", pattern, "error", err)
			continue
		}
		if !matched {
			continue
		}
		if bestPattern == "" || len(normalizedPattern) > len(bestPattern) || (len(normalizedPattern) == len(bestPattern) && normalizedPattern < bestPattern) {
			bestPattern = normalizedPattern
			bestImage = image
		}
	}
	return bestImage
}

func maxInt(a, b int) int {
	if b > a {
		return b
	}
	return a
}

func maxCUDAVersionString(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	// Component-wise integer compare so "12.10" > "12.8" (float parsing would
	// treat "12.10" as 12.1, silently mis-ordering future CUDA versions).
	ac := cudaVersionComponents(a)
	bc := cudaVersionComponents(b)
	if ac != nil && bc != nil {
		if cmpVersionComponents(bc, ac) > 0 {
			return b
		}
		return a
	}
	// Fallback to string compare for malformed input.
	if b > a {
		return b
	}
	return a
}

// cudaVersionComponents splits a CUDA-style "major.minor[.patch]" string into
// integer components. Returns nil when any component is empty or non-numeric.
func cudaVersionComponents(s string) []int {
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		if p == "" {
			return nil
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

// cmpVersionComponents returns -1/0/+1 comparing two component slices.
// Missing trailing components compare as 0.
func cmpVersionComponents(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(a) {
			ai = a[i]
		}
		if i < len(b) {
			bi = b[i]
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// groupMaxComputeCap reduces per-job arch caps into the most restrictive
// (lowest) cap for a group, reading jobs.max_compute_cap. Empty caps are
// lazily resolved against the current torch pin and persisted back, so
// legacy rows and post-refresh retries pick up a fresh value here rather
// than requiring a separate refresh call.
//
// Caps share by (localDir, command), so the resolveCache avoids re-reading
// uv.lock per job in groups of identical scripts.
func groupMaxComputeCap(database *sql.DB, jobs []*db.Job) string {
	type capKey struct{ dir, cmd string }
	resolveCache := map[capKey]string{}
	resolveAndPersist := func(job *db.Job) string {
		localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
		k := capKey{localDir, job.Command}
		if v, ok := resolveCache[k]; ok {
			return v
		}
		v := placement.ResolveJobMaxComputeCapForPersistence(localDir, job.Command)
		resolveCache[k] = v
		if v != "" && database != nil {
			if err := db.SetJobMaxComputeCap(database, job.ID, v); err != nil {
				slog.Warn("failed to persist resolved max_compute_cap",
					"component", "campaign", "job_id", job.ID, "error", err)
			} else {
				job.MaxComputeCap = v
			}
		}
		return v
	}

	minCap := ""
	for _, job := range jobs {
		cap := job.MaxComputeCap
		if cap == "" {
			cap = resolveAndPersist(job)
		} else if cap != placement.MaxComputeCapAny {
			if resolved := resolveAndPersist(job); resolved != "" && resolved != placement.MaxComputeCapAny && resolved != cap {
				cap = resolved
			}
		}
		switch {
		case cap == "":
			// Backfill failed (source unreadable). Don't constrain this job;
			// the group's filter still reflects the resolved jobs.
			slog.Warn("max_compute_cap unresolved at launch; arch filter not applied for job",
				"component", "campaign", "job_id", job.ID)
		case cap == placement.MaxComputeCapAny:
			return "" // group is explicitly unbounded
		default:
			if minCap == "" || placement.CompareComputeCap(cap, minCap) < 0 {
				minCap = cap
			}
		}
	}
	return minCap
}

func groupMinComputeCap(jobs []*db.Job) string {
	type capKey struct{ dir string }
	resolveCache := map[capKey]string{}
	maxMinCap := ""
	for _, job := range jobs {
		if job == nil {
			continue
		}
		localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
		k := capKey{localDir}
		cap, ok := resolveCache[k]
		if !ok {
			cap = placement.MinComputeCapForJob(localDir)
			resolveCache[k] = cap
		}
		if cap != "" && (maxMinCap == "" || placement.CompareComputeCap(cap, maxMinCap) > 0) {
			maxMinCap = cap
		}
	}
	return maxMinCap
}

// GroupMaxComputeCap reduces per-job arch caps into the most restrictive cap
// for a group. It is exported for orchestration paths that build ad hoc groups
// outside instance planning.
func GroupMaxComputeCap(database *sql.DB, jobs []*db.Job) string {
	return groupMaxComputeCap(database, jobs)
}

// GroupMinComputeCap reduces per-job arch lower bounds into the most
// restrictive lower bound for a group.
func GroupMinComputeCap(jobs []*db.Job) string {
	return groupMinComputeCap(jobs)
}

// ResolveJobVastCapAdd returns the Vast.ai-only capabilities to request for a
// job from PEP 723 [tool.weft] metadata. Returns nil if unspecified.
func ResolveJobVastCapAdd(localDir, command string) []string {
	meta, err := dataloc.ScanScriptMeta(localDir, command)
	if err != nil {
		slog.Warn("script metadata error in vast cap-add resolution", "component", "campaign", "error", err)
		return nil
	}
	if meta == nil || len(meta.VastCapAdd) == 0 {
		return nil
	}
	return append([]string(nil), meta.VastCapAdd...)
}

func mergeVastCapAdd(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := append([]string(nil), a...)
	for _, capVal := range b {
		if !slices.Contains(out, capVal) {
			out = append(out, capVal)
		}
	}
	return out
}

func normalizeRunpodGroupImage(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return cloud.DefaultRunpodImage
	}
	if strings.HasPrefix(strings.ToLower(image), "runpod/") {
		return image
	}
	return cloud.DefaultRunpodImage
}

// SourceDirs returns the unique local absolute paths for all jobs in the group.
// Each path is resolved from the job's working directory to a local absolute path.
// Paths that cannot be resolved (relative, unrecognized prefix) are skipped.
func (g InstanceGroup) SourceDirs() []string {
	seen := make(map[string]bool)
	var dirs []string
	for _, job := range g.Jobs {
		d := workdir.ResolveLocal(job.EffectiveWorkingDir())
		if d == "" || seen[d] {
			continue
		}
		if workdir.IsContainerPath(d) {
			slog.Warn("skipping container path in source dirs",
				"job_id", job.ID, "path", d)
			continue
		}
		seen[d] = true
		dirs = append(dirs, d)
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

// HasPreemptibleJob returns true if any job in the group explicitly allows
// interruptible/preemptible placement.
func (g InstanceGroup) HasPreemptibleJob() bool {
	if g.Preemptible {
		return true
	}
	for _, job := range g.Jobs {
		if job.UsesPreemptiblePlacement() {
			return true
		}
	}
	return false
}

// GPUSpec returns a human-readable GPU spec string for the group.
func (g InstanceGroup) GPUSpec() string {
	var spec string
	switch {
	case g.GPUClass != "" && g.GPUMemGB > 0:
		spec = g.GPUClass + " ≥" + strconv.Itoa(g.GPUMemGB) + "GB"
	case g.GPUClass != "":
		spec = g.GPUClass
	case g.GPUMemGB > 0:
		spec = "≥" + strconv.Itoa(g.GPUMemGB) + "GB"
	default:
		spec = "GPU"
	}
	return spec
}
