package placement

import (
	"fmt"
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

// RuntimeFloor is the resolved minimum NVIDIA runtime requirement for a job,
// together with the provenance of its CUDA floor so displays can say where
// the floor came from (e.g. "Driver floor: >=525 (CUDA >=12.0, from torch
// 2.9.1+cu128)").
type RuntimeFloor struct {
	Req cloud.ImageRequirements
	// CUDAOrigin labels the source that supplied the winning MinCUDAVersion:
	// a torch pin, a library dependency floor, script metadata, or project
	// config. Empty when no CUDA floor applies.
	CUDAOrigin string
	// DriverExplicit reports that MinDriverVersion was given explicitly
	// (script metadata or .weft.toml) rather than backfilled from the CUDA
	// floor.
	DriverExplicit bool
}

// MergeInferred max-merges an inferred requirement (torch pin, library
// floor) into the floor, recording origin when the contribution wins the
// CUDA floor. Inferred sources never set DriverExplicit.
func (rf *RuntimeFloor) MergeInferred(req cloud.ImageRequirements, origin string) {
	before := rf.Req.MinCUDAVersion
	rf.Req = imagereq.Merge(rf.Req, req)
	if rf.Req.MinCUDAVersion != before {
		rf.CUDAOrigin = origin
	}
}

// ApplyExplicit applies a user-supplied min-driver / cuda-driver-min pair on
// top of the inferred floor. Unlike inferred sources, explicit values
// REPLACE rather than max-merge — the user may lower the floor — and the
// cuda-driver-min spellings "any"/"none" clear it entirely. cuda-driver-min
// accepts versions ("12.4"), torch wheel tags ("cu128"), and generation
// names ("hopper") via ParseCUDADriverFloor. Call FinalizeDriver afterwards
// to recompute the driver backfill.
func (rf *RuntimeFloor) ApplyExplicit(minDriver, minCUDA, source string) error {
	if minCUDA = strings.TrimSpace(minCUDA); minCUDA != "" {
		parsed, err := ParseCUDADriverFloor(minCUDA)
		if err != nil {
			return fmt.Errorf("%s cuda-driver-min: %w", source, err)
		}
		rf.Req.MinCUDAVersion = parsed
		if parsed == "" {
			rf.CUDAOrigin = ""
		} else {
			rf.CUDAOrigin = source
		}
	}
	if minDriver = strings.TrimSpace(minDriver); minDriver != "" {
		n, err := strconv.Atoi(minDriver)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s min-driver must be a positive integer, got %q", source, minDriver)
		}
		rf.Req.MinDriverVersion = n
		rf.DriverExplicit = true
	}
	return nil
}

// FinalizeDriver derives the driver floor from the CUDA floor unless an
// explicit min-driver was given. Unlike imagereq.BackfillDriverFromCUDA
// (whose raise-only semantics fit image-label merges), this REPLACES the
// derived driver value, so an explicit cuda-driver-min that lowered the CUDA
// floor lowers the driver floor with it. Idempotent; safe to call again
// after a later ApplyExplicit.
func (rf *RuntimeFloor) FinalizeDriver() {
	if rf.DriverExplicit {
		return
	}
	rf.Req.MinDriverVersion = 0
	if rf.Req.MinCUDAVersion != "" {
		rf.Req.MinDriverVersion = imagereq.MinDriverForCUDA(rf.Req.MinCUDAVersion)
	}
}

// ApplyCLIOverride applies the --cuda-driver-min CLI value (the
// highest-precedence explicit level: replaces the inferred/metadata floor,
// may lower it, "any" clears it) and re-derives the driver floor. One method
// so every resolution path uses the same label and the
// ApplyExplicit-then-FinalizeDriver pairing cannot be forgotten.
func (rf *RuntimeFloor) ApplyCLIOverride(minCUDA string) error {
	if strings.TrimSpace(minCUDA) == "" {
		return nil
	}
	if err := rf.ApplyExplicit("", minCUDA, "--cuda-driver-min"); err != nil {
		return err
	}
	rf.FinalizeDriver()
	return nil
}

// MinRuntimeFloorForJob resolves local, non-network NVIDIA runtime
// requirements for a job. It mirrors the cloud planner's local sources:
// project .weft.toml, script metadata, torch lockfile, and inline dependency
// declarations. Image label fetching remains in the cloud campaign path.
//
// The torch-pin contribution uses the CUDA *family* floor
// (dataloc.CUDAFamilyFloor): pip wheels bundle their CUDA runtime and run on
// any same-major driver under minor-version compatibility, so a cu128 pin
// implies CUDA >=12.0 / driver >=525, not the 12.8 toolkit floor. Library
// floors (e.g. vLLM) are cited toolkit requirements and stay exact.
//
// Explicit sources are applied lowest-precedence first (.weft.toml [cloud],
// then script [tool.weft]); each replaces the CUDA floor and may lower or
// clear it. The CLI --cuda-driver-min level is applied by the caller on top.
//
// The returned error reports unparseable explicit requirements (script
// metadata or .weft.toml); the floor still reflects the sources that parsed.
func MinRuntimeFloorForJob(dir, command string) (RuntimeFloor, error) {
	var rf RuntimeFloor
	var parseErr error
	if pin := dataloc.ScanTorchPin(dir); pin != nil {
		if family := dataloc.CUDAFamilyFloor(pin.CudaVariant); family != "" {
			major, _, _ := strings.Cut(family, ".")
			origin := fmt.Sprintf("torch %s+%s (CUDA %s.x family)", pin.Version, pin.CudaVariant, major)
			rf.MergeInferred(cloud.ImageRequirements{MinCUDAVersion: family}, origin)
		}
	}
	deps := append([]dataloc.DepSpec{}, dataloc.ScanUVRunWith(command)...)
	deps = append(deps, dataloc.ParseDepSpecs(dataloc.ScanScriptDependencies(dir, command))...)
	if libCUDA := dataloc.LibraryMinCUDAFromDeps(deps); libCUDA != "" {
		rf.MergeInferred(cloud.ImageRequirements{MinCUDAVersion: libCUDA}, "library dependency CUDA floor")
	}
	if minDriver, minCUDA := config.ProjectCloudRequirements(dir); minDriver != "" || minCUDA != "" {
		if err := rf.ApplyExplicit(minDriver, minCUDA, ".weft.toml [cloud]"); err != nil {
			parseErr = err
		}
	}
	if meta, err := dataloc.ScanScriptMeta(dir, command); err == nil && meta != nil && (meta.MinDriver != "" || meta.MinCUDA != "") {
		if err := rf.ApplyExplicit(meta.MinDriver, meta.MinCUDA, "script [tool.weft]"); err != nil {
			parseErr = err
		}
	}
	rf.FinalizeDriver()
	return rf, parseErr
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
