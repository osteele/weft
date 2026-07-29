package placement

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/gpucatalog"
	"github.com/osteele/weft/internal/inventory"
)

// GPUGeneration represents an ordered GPU generation/architecture.
// Higher values are newer. NVIDIA and Apple are separate families
// and cannot be compared across families.
type GPUGeneration int

const (
	GenUnknown GPUGeneration = iota

	// NVIDIA generations (ordered oldest to newest). Pre-Volta generations
	// are listed for compute-cap recognition only; torch >= 2.5 cu118+ no
	// longer ships kernels for sm < 7.0, so placement uses these mainly to
	// REJECT Maxwell/Pascal offers when a torch-derived min-cap is set.
	GenMaxwell     = GPUGeneration(gpucatalog.GenMaxwell)
	GenPascal      = GPUGeneration(gpucatalog.GenPascal)
	GenVolta       = GPUGeneration(gpucatalog.GenVolta)
	GenTuring      = GPUGeneration(gpucatalog.GenTuring)
	GenAmpere      = GPUGeneration(gpucatalog.GenAmpere)
	GenAdaLovelace = GPUGeneration(gpucatalog.GenAdaLovelace)
	GenHopper      = GPUGeneration(gpucatalog.GenHopper)
	GenBlackwell   = GPUGeneration(gpucatalog.GenBlackwell)

	// Apple generations (separate family, ordered oldest to newest)
	GenAppleM1 GPUGeneration = 100
	GenAppleM2 GPUGeneration = 101
	GenAppleM3 GPUGeneration = 102
	GenAppleM4 GPUGeneration = 103
)

func (g GPUGeneration) isNVIDIA() bool {
	return g >= GenMaxwell && g <= GenBlackwell
}

func (g GPUGeneration) isApple() bool {
	return g >= GenAppleM1 && g <= GenAppleM4
}

// appleClassToGeneration maps normalized Apple GPU class names to generation.
var appleClassToGeneration = map[string]GPUGeneration{
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

var knownGPUClasses = func() []string {
	classes := make([]string, 0, len(appleClassToGeneration)+len(gpucatalog.KnownNormalizedClasses()))
	for class := range appleClassToGeneration {
		classes = append(classes, class)
	}
	classes = append(classes, gpucatalog.KnownNormalizedClasses()...)
	return classes
}()

// generationNames maps user-facing generation names to their generation.
var generationNames = buildGenerationNames()

// generationMinCUDA maps GPU generations to the minimum CUDA toolkit version
// required to compile kernels for that architecture.
var generationMinCUDA = map[GPUGeneration]float64{
	GenMaxwell:     6.5,
	GenPascal:      8.0,
	GenVolta:       9.0,
	GenTuring:      10.0,
	GenAmpere:      11.0,
	GenAdaLovelace: 11.8,
	GenHopper:      12.0,
	GenBlackwell:   12.8,
}

// MinCUDAForGPU returns the minimum CUDA toolkit version needed for a GPU,
// identified by its name as it appears in cloud offers (e.g., "RTX 5090").
// Returns 0 if the GPU is unknown or doesn't require a specific CUDA version.
func MinCUDAForGPU(gpuName string) float64 {
	normalized := inventory.NormalizeGPUClass(gpuName)
	gen := generationOf(normalized)
	if gen == GenUnknown {
		return 0
	}
	return generationMinCUDA[gen]
}

// ParseCUDADriverFloor parses a user-supplied driver/CUDA floor and returns
// it canonicalized as a "MAJOR.MINOR" version string. Accepted inputs:
//   - bare numeric versions: "12", "12.4", "12.8.1" → "12.0", "12.4", "12.8"
//   - torch wheel tags: "cu128", "cu121" → "12.8", "12.1"
//   - generation names: "hopper", "blackwell", "ada", "ampere" → mapped via
//     generationMinCUDA (e.g., "blackwell" → "12.8")
//   - "any" / "none" / "" → empty string (no floor)
func ParseCUDADriverFloor(raw string) (string, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" || s == "any" || s == "none" {
		return "", nil
	}
	if strings.HasPrefix(s, "cu") {
		rest := strings.TrimPrefix(s, "cu")
		if n, err := strconv.Atoi(rest); err == nil && n >= 100 {
			return fmt.Sprintf("%d.%d", n/10, n%10), nil
		}
	}
	if v, ok := parseSemverPair(s); ok {
		return v, nil
	}
	if gen, ok := generationNames[strings.ReplaceAll(s, " ", "")]; ok && gen.isNVIDIA() {
		if v, ok := generationMinCUDA[gen]; ok {
			major := int(v)
			minor := int((v-float64(major))*10 + 0.5)
			return fmt.Sprintf("%d.%d", major, minor), nil
		}
		return "", nil
	}
	return "", fmt.Errorf("unrecognized CUDA driver floor: %s (expected version like \"12.4\" or generation name like \"hopper\")", raw)
}

// parseSemverPair parses "12", "12.4", or "12.4.1" into "MAJOR.MINOR".
// Returns the canonical string and ok=true; ok=false signals "not a version".
func parseSemverPair(s string) (string, bool) {
	parts := strings.SplitN(s, ".", 3)
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return "", false
	}
	minor := 0
	if len(parts) >= 2 && parts[1] != "" {
		m, err := strconv.Atoi(parts[1])
		if err != nil || m < 0 {
			return "", false
		}
		minor = m
	}
	return fmt.Sprintf("%d.%d", major, minor), true
}

// MinCUDAForConstraint returns the minimum CUDA toolkit version implied by a
// --gpu-class constraint (model, generation, or generation+). For a family
// constraint, returns the max over members in generationMinCUDA: a family
// admits every member, so the image must support the most demanding
// architecture the placer might pick.
func MinCUDAForConstraint(gpuClass string) float64 {
	c := ParseGPUConstraint(gpuClass)
	switch c.mode {
	case constraintExactModel:
		return MinCUDAForGPU(gpuClass)
	case constraintExactGen, constraintMinGen:
		return generationMinCUDA[c.generation]
	case constraintFamily:
		checker, ok := familyCheckers[c.normalized]
		if !ok {
			return 0
		}
		var maxCUDA float64
		for gen, v := range generationMinCUDA {
			if checker(gen) && v > maxCUDA {
				maxCUDA = v
			}
		}
		return maxCUDA
	default:
		return 0
	}
}

// generationOf returns the generation for a normalized GPU class name.
func generationOf(normalizedClass string) GPUGeneration {
	if gen, ok := appleClassToGeneration[normalizedClass]; ok {
		return gen
	}
	if gen, ok := gpucatalog.GenerationForNormalizedClass(normalizedClass); ok {
		return GPUGeneration(gen)
	}
	for _, alias := range gpucatalog.NormalizedAliases(normalizedClass) {
		if gen, ok := gpucatalog.GenerationForNormalizedClass(alias); ok {
			return GPUGeneration(gen)
		}
	}
	return GenUnknown
}

func buildGenerationNames() map[string]GPUGeneration {
	names := make(map[string]GPUGeneration, len(gpucatalog.GenerationNameToGeneration)+4)
	for name, gen := range gpucatalog.GenerationNameToGeneration {
		names[name] = GPUGeneration(gen)
	}
	names["applem1"] = GenAppleM1
	names["applem2"] = GenAppleM2
	names["applem3"] = GenAppleM3
	names["applem4"] = GenAppleM4
	return names
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
	mode gpuConstraintMode
	// skuMemGB is the capacity named inside the class ("a100-sxm4-80gb" -> 80),
	// 0 when none was given. It is an identity axis, not a floor: a floor is
	// spelled ">=NGB" and lands in Constraints.GPUMemGB instead.
	skuMemGB   int
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

	// Fall back to exact model match ('+' on unknown model treated as exact).
	// A memory token inside the class names a SKU rather than a floor, and the
	// name axis cannot answer it (part names carry no capacity), so it is kept
	// as its own axis and checked against the device's reported memory.
	base, skuMemGB := gpucatalog.SplitTrailingMemorySuffix(s)
	if skuMemGB > 0 {
		return GPUConstraint{mode: constraintExactModel, normalized: normalizeGPUClass(base), skuMemGB: skuMemGB}
	}
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
		return c.matchesExactNormalized(invNorm)
	case constraintExactGen, constraintMinGen, constraintFamily:
		return c.matchesGeneration(generationOf(invNorm))
	}
	return false
}

// IsFloatable returns true if the constraint does not pin to a specific GPU
// model — i.e., it is a family constraint ("nvidia"), an exact generation
// ("ampere"), or a minimum generation ("ampere+"). Floatable constraints
// allow the strategy engine to select the best GPU within the constraint.
func (c GPUConstraint) IsFloatable() bool {
	return c.mode == constraintFamily || c.mode == constraintExactGen || c.mode == constraintMinGen
}

// Subsumes returns true if every GPU that satisfies other also satisfies c.
// In other words, c is a broader (or equal) constraint than other.
//
// Note: two zero-value GPUConstraints (from parsing "") will return true
// (both are constraintExactModel with normalized ""). Callers that need to
// treat empty strings as "unconstrained" should handle that before calling.
func (c GPUConstraint) Subsumes(other GPUConstraint) bool {
	switch c.mode {
	case constraintFamily:
		// Family subsumes anything within that family.
		switch other.mode {
		case constraintFamily:
			return c.normalized == other.normalized
		case constraintExactGen, constraintMinGen:
			return familyCheckers[c.normalized](other.generation)
		case constraintExactModel:
			gen := generationOf(other.normalized)
			return gen != GenUnknown && familyCheckers[c.normalized](gen)
		}

	case constraintMinGen:
		// "ampere+" subsumes "hopper+" (higher min), exact gen at-or-above,
		// and exact models whose generation is at-or-above.
		switch other.mode {
		case constraintFamily:
			return false
		case constraintMinGen:
			if c.generation.isNVIDIA() != other.generation.isNVIDIA() ||
				c.generation.isApple() != other.generation.isApple() {
				return false
			}
			return c.generation <= other.generation
		case constraintExactGen:
			return c.matchesGeneration(other.generation)
		case constraintExactModel:
			gen := generationOf(other.normalized)
			return gen != GenUnknown && c.matchesGeneration(gen)
		}

	case constraintExactGen:
		// "ampere" (exact) subsumes only exact models within that generation.
		switch other.mode {
		case constraintFamily, constraintMinGen:
			return false
		case constraintExactGen:
			return c.generation == other.generation
		case constraintExactModel:
			gen := generationOf(other.normalized)
			return gen != GenUnknown && gen == c.generation
		}

	case constraintExactModel:
		// Exact model only subsumes itself, on both axes.
		if other.mode == constraintExactModel {
			return c.subsumesSKUMemory(other) && c.matchesExactNormalized(other.normalized)
		}
		return false
	}
	return false
}

// subsumesSKUMemory reports whether c's SKU axis is no narrower than other's. A
// constraint that named no capacity accepts every SKU of the part; one that
// named a capacity accepts only that SKU.
//
// This has to be asked separately now that the capacity lives outside the name:
// "a100-sxm4-40gb" and "a100-sxm4-80gb" share a normalized name, so on the name
// axis alone each subsumes the other, and jobs asking for different hardware
// would merge into a single rental.
func (c GPUConstraint) subsumesSKUMemory(other GPUConstraint) bool {
	return c.skuMemGB == 0 || c.skuMemGB == other.skuMemGB
}

// MatchesGPUFullName returns true if a full nvidia-smi GPU name (e.g.
// "NVIDIA A100-PCIE-80GB") satisfies this constraint. For exact model mode it
// identifies the catalog model in the full name before accepting short aliases;
// this avoids collisions such as "l4" matching "L40" or "L40S". For
// generation modes it identifies the GPU model from the full name and applies
// generation comparison.
func (c GPUConstraint) MatchesGPUFullName(fullName string) bool {
	normFull := normalizeGPUClass(fullName)

	switch c.mode {
	case constraintExactModel:
		return c.matchesExactFullName(normFull)

	case constraintExactGen, constraintMinGen, constraintFamily:
		bestClass := bestKnownGPUClassInFullName(normFull)
		if bestClass == "" {
			// No known model found. For family constraints, check if the name
			// starts with the family prefix (e.g., "nvidia" matches "nvidiageforce..."
			// even when the model name is truncated by nvidia-smi).
			if c.mode == constraintFamily {
				return strings.HasPrefix(normFull, c.normalized)
			}
			return false
		}
		return c.matchesGeneration(generationOf(bestClass))
	}
	return false
}

func (c GPUConstraint) matchesExactFullName(normFull string) bool {
	if c.normalized == "" || normFull == "" {
		return false
	}
	bestClass := bestKnownGPUClassInFullName(normFull)
	if bestClass == "" {
		for _, norm := range gpucatalog.NormalizedEquivalents(c.normalized) {
			if strings.Contains(normFull, norm) {
				return true
			}
		}
		return c.normalized == "h100hbm3" && strings.Contains(normFull, "h100") && strings.Contains(normFull, "hbm3")
	}
	if c.matchesExactNormalized(bestClass) {
		return true
	}
	for _, norm := range gpucatalog.NormalizedEquivalents(c.normalized) {
		if strings.Contains(normFull, norm) && len(norm) > len(bestClass) {
			return true
		}
	}
	return false
}

func bestKnownGPUClassInFullName(normFull string) string {
	if strings.Contains(normFull, "h100") && strings.Contains(normFull, "hbm3") {
		return "h100sxm"
	}
	bestClass := ""
	for _, class := range knownGPUClasses {
		if strings.Contains(normFull, class) && len(class) > len(bestClass) {
			bestClass = class
		}
	}
	return bestClass
}

func (c GPUConstraint) matchesExactNormalized(candidate string) bool {
	return gpucatalog.MatchesPartName(c.normalized, candidate)
}

// matchesSKUMemory checks the identity axis a part name cannot carry. When the
// constraint named a capacity inside the class ("a100-sxm4-80gb"), only that
// part satisfies it — a 40GB A100 SXM4 is different hardware, not a smaller
// instance of the same thing. The rounding and the unknown-is-not-disqualifying
// rule live with the hardware facts so that this and the cloud offer filter
// cannot drift apart on what a SKU match means.
func (c GPUConstraint) matchesSKUMemory(deviceMemGB int) bool {
	return gpucatalog.MatchesSKUMemoryGB(c.normalized, c.skuMemGB, deviceMemGB)
}
