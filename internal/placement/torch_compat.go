package placement

import (
	"sort"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/dataloc"
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
	maj, min, ok := parseTorchMajMin(version)
	if !ok {
		return ""
	}
	cu := strings.ToLower(strings.TrimSpace(cudaVariant))
	if cu == "cpu" || cu == "" {
		// Without CUDA we can't constrain; leave caller to apply other filters.
		return ""
	}
	switch {
	case maj < 2:
		return "8.0" // Ampere — pre-2.0 wheels predate Hopper
	case maj == 2 && min <= 4:
		return "9.0" // Hopper; no Blackwell support before 2.5
	case maj == 2 && min == 5:
		// 2.5 added datacenter Blackwell (sm_100) on cu124+ wheels.
		if cu == "cu124" || cu == "cu126" || cu == "cu128" {
			return "10.0"
		}
		return "9.0"
	case maj == 2 && min == 6:
		// 2.6 broadened sm_100 coverage; consumer/PRO Blackwell (sm_120) on cu128.
		if cu == "cu128" {
			return "12.0"
		}
		return "10.0"
	case maj == 2 && min >= 7:
		// 2.7+ ships sm_120 on cu126/cu128 wheels.
		if cu == "cu126" || cu == "cu128" {
			return "12.0"
		}
		return "10.0"
	case maj >= 3:
		// Future-proof default; conservative — caller can override.
		return "12.0"
	}
	return ""
}

// modelComputeCap maps normalized GPU model fragments to compute capability.
// Used for cases where the gen-default is too coarse — for example, A100 has
// sm_8.0 while consumer Ampere is sm_8.6, and datacenter Blackwell (B100/B200)
// is sm_10.0 while consumer/PRO Blackwell is sm_12.0.
var modelComputeCap = map[string]string{
	// Ampere datacenter
	"a100": "8.0",
	"a30":  "8.0",
	"a40":  "8.6",
	// Hopper
	"h100": "9.0",
	"h200": "9.0",
	// Datacenter Blackwell (sm_100)
	"b100":  "10.0",
	"b200":  "10.0",
	"b300":  "10.0",
	"gb200": "10.0",
}

// generationDefaultComputeCap is the cap to assume for a GPU when only its
// generation is known. For arches with multiple SMs in active use, this picks
// the highest cap we'd reasonably encounter on a Vast offer, since that is
// the conservative choice for an upper-bound filter.
var generationDefaultComputeCap = map[GPUGeneration]string{
	GenVolta:       "7.0",
	GenTuring:      "7.5",
	GenAmpere:      "8.6", // most consumer Ampere; A100 is 8.0
	GenAdaLovelace: "8.9",
	GenHopper:      "9.0",
	GenBlackwell:   "12.0", // RTX PRO / 50-series; B100/B200 split out via modelComputeCap
}

// modelComputeCapKeys is modelComputeCap's keys sorted by descending length
// so that longer fragments (e.g. "gb200") match before shorter prefixes
// (e.g. "b200"). Map iteration order is randomized; this list is the
// canonical traversal order used by ComputeCapForGPU.
var modelComputeCapKeys = sortedByLenDesc(modelComputeCap)

// nvidiaGenerationNamesSorted is nvidiaGenerationNames' keys sorted by
// descending length for the same reason.
var nvidiaGenerationNamesSorted = sortedByLenDesc(nvidiaGenerationNames)

func sortedByLenDesc[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

// ComputeCapForGPU returns the CUDA compute capability of a GPU, identified
// by its name as it appears in cloud offers ("RTX PRO 4500 Blackwell",
// "B200", "A100"). Returns "" if unknown.
func ComputeCapForGPU(gpuName string) string {
	norm := inventory.NormalizeGPUClass(gpuName)
	if cap, ok := modelComputeCap[norm]; ok {
		return cap
	}
	for _, fragment := range modelComputeCapKeys {
		if strings.Contains(norm, fragment) {
			return modelComputeCap[fragment]
		}
	}
	if gen := generationOf(norm); gen != GenUnknown {
		return generationDefaultComputeCap[gen]
	}
	for _, class := range knownGPUClasses {
		if strings.Contains(norm, class) {
			if g := generationOf(class); g != GenUnknown {
				return generationDefaultComputeCap[g]
			}
		}
	}
	// Catches names like "RTX PRO 4500 Blackwell" where the model isn't in the
	// known list but the generation is named explicitly.
	for _, genName := range nvidiaGenerationNamesSorted {
		if strings.Contains(norm, genName) {
			return generationDefaultComputeCap[nvidiaGenerationNames[genName]]
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
	return generationDefaultComputeCap[gen]
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

// parseTorchMajMin parses a torch version like "2.4.1" or "2.6.0+cu128" into
// integer major and minor components. Returns ok=false if it can't.
func parseTorchMajMin(version string) (maj, min int, ok bool) {
	v := strings.TrimSpace(version)
	if v == "" {
		return 0, 0, false
	}
	// Strip local-version suffix ("2.6.0+cu128" -> "2.6.0").
	if idx := strings.Index(v, "+"); idx >= 0 {
		v = v[:idx]
	}
	// Strip pre-release tags (".dev0", "rc1") for our needs.
	for _, sep := range []string{"a", "b", "rc", ".dev", "-"} {
		if idx := strings.Index(v, sep); idx >= 0 {
			v = v[:idx]
		}
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	majN, err1 := strconv.Atoi(parts[0])
	minN, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return majN, minN, true
}
