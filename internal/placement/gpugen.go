package placement

import "strings"

// GPUGeneration represents an ordered GPU generation/architecture.
// Higher values are newer. NVIDIA and Apple are separate families
// and cannot be compared across families.
type GPUGeneration int

const (
	GenUnknown GPUGeneration = iota

	// NVIDIA generations (ordered oldest to newest)
	GenTuring
	GenAmpere
	GenAdaLovelace
	GenHopper
	GenBlackwell

	// Apple generations (separate family, ordered oldest to newest)
	GenAppleM1
	GenAppleM2
	GenAppleM3
	GenAppleM4
)

func (g GPUGeneration) isNVIDIA() bool {
	return g >= GenTuring && g <= GenBlackwell
}

func (g GPUGeneration) isApple() bool {
	return g >= GenAppleM1 && g <= GenAppleM4
}

// gpuClassToGeneration maps normalized GPU class names to their generation.
var gpuClassToGeneration = map[string]GPUGeneration{
	// Turing (RTX 20-series, Quadro RTX, Tesla T4)
	"rtx2080ti": GenTuring,
	"rtx2080":   GenTuring,
	"rtx2070":   GenTuring,
	"rtx2060":   GenTuring,
	"t4":        GenTuring,

	// Ampere (RTX 30-series, A100, A10, A40)
	"rtx3090":  GenAmpere,
	"rtx3080":  GenAmpere,
	"rtx3070":  GenAmpere,
	"rtx3060":  GenAmpere,
	"a100":     GenAmpere,
	"a10":      GenAmpere,
	"a40":      GenAmpere,
	"a10g":     GenAmpere,
	"a100sxm4": GenAmpere,

	// Ada Lovelace (RTX 40-series, L40, L4)
	"rtx4090": GenAdaLovelace,
	"rtx4080": GenAdaLovelace,
	"rtx4070": GenAdaLovelace,
	"rtx4060": GenAdaLovelace,
	"l40":     GenAdaLovelace,
	"l40s":    GenAdaLovelace,
	"l4":      GenAdaLovelace,

	// Hopper (H100, H200)
	"h100":    GenHopper,
	"h100sxm": GenHopper,
	"h200":    GenHopper,

	// Blackwell (B100, B200, GB200)
	"b100":  GenBlackwell,
	"b200":  GenBlackwell,
	"gb200": GenBlackwell,

	// Apple Silicon
	"m1":      GenAppleM1,
	"m1pro":   GenAppleM1,
	"m1max":   GenAppleM1,
	"m1ultra": GenAppleM1,
	"m2":      GenAppleM2,
	"m2pro":   GenAppleM2,
	"m2max":   GenAppleM2,
	"m2ultra": GenAppleM2,
	"m3":      GenAppleM3,
	"m3pro":   GenAppleM3,
	"m3max":   GenAppleM3,
	"m3ultra": GenAppleM3,
	"m4":      GenAppleM4,
	"m4pro":   GenAppleM4,
	"m4max":   GenAppleM4,
}

// generationNames maps user-facing generation names to their generation.
var generationNames = map[string]GPUGeneration{
	"turing":      GenTuring,
	"ampere":      GenAmpere,
	"ada":         GenAdaLovelace,
	"adalovelace": GenAdaLovelace,
	"hopper":      GenHopper,
	"blackwell":   GenBlackwell,
	"applem1":     GenAppleM1,
	"applem2":     GenAppleM2,
	"applem3":     GenAppleM3,
	"applem4":     GenAppleM4,
}

// generationOf returns the generation for a normalized GPU class name.
func generationOf(normalizedClass string) GPUGeneration {
	if gen, ok := gpuClassToGeneration[normalizedClass]; ok {
		return gen
	}
	return GenUnknown
}

type gpuConstraintMode int

const (
	constraintExactModel gpuConstraintMode = iota
	constraintExactGen
	constraintMinGen
	constraintFamily
)

// familyCheckers maps family names to their membership test function.
var familyCheckers = map[string]func(GPUGeneration) bool{
	"nvidia": GPUGeneration.isNVIDIA,
	"apple":  GPUGeneration.isApple,
}

// GPUConstraint represents a parsed --gpu-class value.
type GPUConstraint struct {
	mode       gpuConstraintMode
	normalized string        // normalizeGPUClass of the base (without '+')
	generation GPUGeneration // resolved generation (for gen/min-gen modes)
}

// ParseGPUConstraint parses a --gpu-class value into a constraint.
// Examples:
//
//	"a100"    → exact model match
//	"ampere"  → exact generation match
//	"ampere+" → minimum generation (Ampere or newer)
//	"a100+"   → minimum generation (promotes model to its generation)
func ParseGPUConstraint(s string) GPUConstraint {
	minMode := strings.HasSuffix(s, "+")
	if minMode {
		s = strings.TrimSuffix(s, "+")
	}

	norm := normalizeGPUClass(s)

	// Check if it's a family name (e.g., "nvidia", "apple")
	if _, ok := familyCheckers[norm]; ok {
		return GPUConstraint{mode: constraintFamily, normalized: norm}
	}

	// Check if it's a generation name
	if gen, ok := generationNames[norm]; ok {
		mode := constraintExactGen
		if minMode {
			mode = constraintMinGen
		}
		return GPUConstraint{mode: mode, normalized: norm, generation: gen}
	}

	// Check if it's a known GPU model — if '+' was used, promote to generation
	if minMode {
		if gen := generationOf(norm); gen != GenUnknown {
			return GPUConstraint{mode: constraintMinGen, normalized: norm, generation: gen}
		}
	}

	// Fall back to exact model match ('+' on unknown model treated as exact)
	return GPUConstraint{mode: constraintExactModel, normalized: norm}
}

// matchesGeneration checks whether invGen satisfies the constraint's generation
// requirement. For exact-gen mode it requires equality; for min-gen mode it
// requires invGen >= the constraint generation within the same family; for
// family mode it checks family membership.
func (c GPUConstraint) matchesGeneration(invGen GPUGeneration) bool {
	if invGen == GenUnknown {
		return false
	}
	switch c.mode {
	case constraintExactGen:
		return invGen == c.generation
	case constraintFamily:
		return familyCheckers[c.normalized](invGen)
	default:
		// constraintMinGen: block cross-family comparison
		if c.generation.isNVIDIA() != invGen.isNVIDIA() || c.generation.isApple() != invGen.isApple() {
			return false
		}
		return invGen >= c.generation
	}
}

// MatchesGPU returns true if the inventory GPU class satisfies this constraint.
func (c GPUConstraint) MatchesGPU(inventoryClass string) bool {
	invNorm := normalizeGPUClass(inventoryClass)

	switch c.mode {
	case constraintExactModel:
		return invNorm == c.normalized
	case constraintExactGen, constraintMinGen, constraintFamily:
		return c.matchesGeneration(generationOf(invNorm))
	}
	return false
}

// MatchesGPUFullName returns true if a full nvidia-smi GPU name (e.g.
// "NVIDIA A100-PCIE-80GB") satisfies this constraint. For exact model mode
// it uses substring matching on the normalized full name. For generation
// modes it identifies the GPU model from the full name and applies generation
// comparison.
func (c GPUConstraint) MatchesGPUFullName(fullName string) bool {
	normFull := normalizeGPUClass(fullName)

	switch c.mode {
	case constraintExactModel:
		return strings.Contains(normFull, c.normalized)

	case constraintExactGen, constraintMinGen, constraintFamily:
		// Identify the GPU model by finding which known class is a substring
		// of the normalized full name. Use the longest match to avoid e.g.
		// "a10" matching before "a100".
		bestClass := ""
		for class := range gpuClassToGeneration {
			if strings.Contains(normFull, class) && len(class) > len(bestClass) {
				bestClass = class
			}
		}
		if bestClass == "" {
			// No known model found. For family constraints, check if the name
			// starts with the family prefix (e.g., "nvidia" matches "nvidiageforce..."
			// even when the model name is truncated by nvidia-smi).
			if c.mode == constraintFamily {
				return strings.HasPrefix(normFull, c.normalized)
			}
			return false
		}
		return c.matchesGeneration(gpuClassToGeneration[bestClass])
	}
	return false
}
