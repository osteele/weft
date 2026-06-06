package placement

import (
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/gpucatalog"
	"github.com/osteele/weft/internal/imagereq"
	"github.com/osteele/weft/internal/inventory"
)

// Compute capabilities are stored as strings ("9.0", "10.0", "12.0") so they
// round-trip cleanly through configs and JSON. Comparison is done numerically
// via CompareComputeCap.

// TorchMaxComputeCap returns the highest CUDA compute capability supported by
// prebuilt PyTorch wheels for (version, cudaVariant). An empty string means
// the bound is unknown — callers should treat that as "no inferred filter".
//
// version is a torch semver string like "2.4.1". cudaVariant is the wheel's
// CUDA tag like "cu121", "cu124", "cu126", "cu128", or "" / "cpu".
//
// Source: PyTorch release notes and each release's TORCH_CUDA_ARCH_LIST. This
// table covers prebuilt wheels published at download.pytorch.org. Source-built
// or nightly wheels are out of scope; users override via [tool.weft]
// gpu-arch-max = "any" or an explicit cap.
func TorchMaxComputeCap(version, cudaVariant string) string {
	return dataloc.TorchMaxComputeCap(version, cudaVariant)
}

// TorchMinComputeCap returns the lowest CUDA compute capability supported by
// prebuilt PyTorch wheels for (version, cudaVariant). Empty string means no
// inferred lower bound.
func TorchMinComputeCap(version, cudaVariant string) string {
	return dataloc.TorchMinComputeCap(version, cudaVariant)
}

// ComputeCapForGPU returns the CUDA compute capability of a GPU, identified
// by its name as it appears in cloud offers ("RTX PRO 4500 Blackwell",
// "B200", "A100"). Returns "" if unknown.
func ComputeCapForGPU(gpuName string) string {
	norm := inventory.NormalizeGPUClass(gpuName)
	if cap := gpucatalog.ComputeCapForGPU(gpuName); cap != "" {
		return cap
	}
	for _, class := range knownGPUClasses {
		if strings.Contains(norm, class) {
			if cap := gpucatalog.ComputeCapForGPU(class); cap != "" {
				return cap
			}
		}
	}
	return ""
}

// CompareComputeCap returns -1, 0, or +1 by parsing strings like "9.0".
// Empty strings sort as smallest (treated as "unknown lower bound"). Malformed
// values also sort as smallest so callers fall through to their default.
func CompareComputeCap(a, b string) int {
	af, aok := parseComputeCap(a)
	bf, bok := parseComputeCap(b)
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	case af < bf:
		return -1
	case af > bf:
		return 1
	}
	return 0
}

// ArchNameToMaxCap maps a named arch ("ampere", "hopper", "blackwell", "any")
// to the highest cap admitted by that name. "any" -> "" (no bound). Unknown
// names also return "".
func ArchNameToMaxCap(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" || n == "any" {
		return ""
	}
	gen, ok := generationNames[normalizeGPUClass(n)]
	if !ok {
		return ""
	}
	if gen.isNVIDIA() {
		return gpucatalog.GenerationDefaultComputeCap(gpucatalog.Generation(gen))
	}
	return ""
}

// hostHasGPUWithinCap returns true if any GPU on the host has a known compute
// capability at or below maxCap, or has an unknown cap (which we accept on
// the assumption the bound cannot be proven violated).
func hostHasGPUWithinCap(host inventory.HostSpec, maxCap string) bool {
	for _, gpu := range host.GPUs {
		gpuCap := ComputeCapForGPU(gpu.Class)
		if gpuCap == "" || CompareComputeCap(gpuCap, maxCap) <= 0 {
			return true
		}
	}
	return false
}

// MaxComputeCapForJob resolves the GPU compute-capability upper bound for a
// job whose source lives in dir. Resolution order:
//
//  1. An explicit gpu-arch-max from script metadata wins. "any" disables
//     filtering. A numeric cap ("9.0") is used directly. A generation name
//     ("hopper") is mapped via ArchNameToMaxCap.
//  2. Otherwise infer from the project's torch pin
//     (dataloc.ScanTorchPin + TorchMaxComputeCap).
//  3. If neither yields a bound, returns "" (no filter).
func MaxComputeCapForJob(archMax, dir string) string {
	override := strings.ToLower(strings.TrimSpace(archMax))
	switch {
	case override == "any":
		return ""
	case override == "":
		// fall through to inference
	default:
		if _, ok := parseComputeCap(override); ok {
			return override
		}
		if cap := ArchNameToMaxCap(override); cap != "" {
			return cap
		}
		return ""
	}
	pin := dataloc.ScanTorchPin(dir)
	if pin == nil {
		return ""
	}
	return TorchMaxComputeCap(pin.Version, pin.CudaVariant)
}

// MinComputeCapForJob resolves the GPU compute-capability lower bound for a
// job whose source lives in dir. It is inferred from the project's torch pin;
// explicit gpu-arch-max metadata only controls the upper bound.
func MinComputeCapForJob(dir string) string {
	pin := dataloc.ScanTorchPin(dir)
	if pin == nil {
		return ""
	}
	return TorchMinComputeCap(pin.Version, pin.CudaVariant)
}

// MinRuntimeRequirementsForJob resolves local, non-network NVIDIA runtime
// requirements for a job. It mirrors the cloud planner's local sources:
// project .weft.toml, script metadata, torch lockfile, and inline dependency
// declarations. Image label fetching remains in the cloud campaign path.
func MinRuntimeRequirementsForJob(dir, command string) cloud.ImageRequirements {
	var req cloud.ImageRequirements
	if minDriver, minCUDA := config.ProjectCloudRequirements(dir); minDriver != "" || minCUDA != "" {
		if explicit, err := imagereq.Explicit(minDriver, minCUDA); err == nil {
			req = imagereq.Merge(req, explicit)
		}
	}
	if meta, err := dataloc.ScanScriptMeta(dir, command); err == nil && meta != nil {
		if explicit, err := imagereq.Explicit(meta.MinDriver, meta.MinCUDA); err == nil {
			req = imagereq.Merge(req, explicit)
		}
	}
	if torchCUDA := dataloc.TorchMinCUDAVersion(dir); torchCUDA != "" {
		req = imagereq.Merge(req, cloud.ImageRequirements{MinCUDAVersion: torchCUDA})
	}
	deps := append([]dataloc.DepSpec{}, dataloc.ScanUVRunWith(command)...)
	deps = append(deps, dataloc.ParseDepSpecs(dataloc.ScanScriptDependencies(dir, command))...)
	if libCUDA := dataloc.LibraryMinCUDAFromDeps(deps); libCUDA != "" {
		req = imagereq.Merge(req, cloud.ImageRequirements{MinCUDAVersion: libCUDA})
	}
	return imagereq.BackfillDriverFromCUDA(req)
}

// MinDriverForCUDAVersion returns the NVIDIA driver-major floor for a CUDA
// compatibility version, or zero when the mapping is unknown.
func MinDriverForCUDAVersion(cuda string) int {
	return imagereq.MinDriverForCUDA(cuda)
}

// MaxComputeCapAny is the persisted-cap sentinel for "explicitly unbounded".
const MaxComputeCapAny = dataloc.MaxComputeCapAny

// ResolveMaxComputeCapForPersistence returns the three-state encoding stored
// on jobs.max_compute_cap:
//
//	MaxComputeCapAny ("any")  explicitly unbounded
//	"X.Y"                     concrete numeric cap (e.g. "10.0", "12.0")
//	""                        torch was pinned but no cap could be derived
//	                          (caller decides whether to warn or retry)
//
// Distinguishes "explicitly unbounded" from "unresolved" so readers can lazily
// backfill empty caps without conflating them with intentional no-bound jobs.
func ResolveMaxComputeCapForPersistence(archMax, dir string) string {
	return dataloc.ResolveTorchMaxComputeCapForPersistence(archMax, dir)
}

// ResolveJobMaxComputeCapForPersistence reads the script's gpu-arch-max
// override (if any) and returns the persistence-encoded cap. Used at submit,
// and again as a lazy backfill on the launch path for legacy/refreshed rows.
func ResolveJobMaxComputeCapForPersistence(localDir, command string) string {
	archMax := ""
	if meta, err := dataloc.ScanScriptMeta(localDir, command); err == nil && meta != nil {
		archMax = meta.GPUArchMax
	}
	return ResolveMaxComputeCapForPersistence(archMax, localDir)
}

// parseComputeCap parses "9.0", "10.0", "12.0" etc. into a float. Returns
// (0, false) for empty or malformed input.
func parseComputeCap(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
