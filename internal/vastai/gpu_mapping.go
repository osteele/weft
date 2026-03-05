package vastai

import (
	"strings"
	"unicode"
)

// gpuClassToVastaiNames maps normalized weft GPU class names to Vast.ai gpu_name values.
// Vast.ai gpu_name is case-sensitive and uses specific naming (e.g. "RTX 4090", "A100 PCIE").
var gpuClassToVastaiNames = map[string][]string{
	// Turing
	"rtx2080ti": {"RTX 2080 Ti"},
	"rtx2080":   {"RTX 2080"},
	"rtx2070":   {"RTX 2070"},
	"rtx2060":   {"RTX 2060"},
	"t4":        {"Tesla T4"},

	// Ampere
	"rtx3090": {"RTX 3090"},
	"rtx3080": {"RTX 3080"},
	"rtx3070": {"RTX 3070"},
	"rtx3060": {"RTX 3060"},
	"a100":    {"A100 PCIE", "A100 SXM4", "A100X"},
	"a10":     {"A10"},
	"a40":     {"A40"},
	"a10g":    {"A10G"},

	// Ada Lovelace
	"rtx4090": {"RTX 4090"},
	"4090":    {"RTX 4090"},
	"rtx4080": {"RTX 4080"},
	"4080":    {"RTX 4080"},
	"rtx4070": {"RTX 4070"},
	"4070":    {"RTX 4070"},
	"rtx4060": {"RTX 4060"},
	"4060":    {"RTX 4060"},
	"l40":     {"L40"},
	"l40s":    {"L40S"},
	"l4":      {"L4"},

	// Hopper
	"h100":    {"H100 SXM", "H100 NVL", "H100 PCIE"},
	"h100sxm": {"H100 SXM"},
	"h200":    {"H200", "H200 NVL"},

	// Blackwell
	"b100":  {"B100"},
	"b200":  {"B200", "B200 NVL"},
	"gb200": {"GB200"},
}

// gpuGeneration represents an ordered GPU generation for Vast.ai filtering.
type gpuGeneration int

const (
	genUnknown gpuGeneration = iota
	genTuring
	genAmpere
	genAdaLovelace
	genHopper
	genBlackwell
)

// generationGPUNames maps each generation to all Vast.ai gpu_name values in that generation.
var generationGPUNames = map[gpuGeneration][]string{
	genTuring:      {"RTX 2080 Ti", "RTX 2080", "RTX 2070", "RTX 2060", "Tesla T4"},
	genAmpere:      {"RTX 3090", "RTX 3080", "RTX 3070", "RTX 3060", "A100 PCIE", "A100 SXM4", "A100X", "A10", "A40", "A10G"},
	genAdaLovelace: {"RTX 4090", "RTX 4080", "RTX 4070", "RTX 4060", "L40", "L40S", "L4"},
	genHopper:      {"H100 SXM", "H100 NVL", "H100 PCIE", "H200", "H200 NVL"},
	genBlackwell:   {"B100", "B200", "B200 NVL", "GB200"},
}

// genNames maps user-facing generation names to their generation.
var genNames = map[string]gpuGeneration{
	"turing":      genTuring,
	"ampere":      genAmpere,
	"ada":         genAdaLovelace,
	"adalovelace": genAdaLovelace,
	"hopper":      genHopper,
	"blackwell":   genBlackwell,
}

// gpuClassToGen maps normalized GPU class names to their generation.
var gpuClassToGen = map[string]gpuGeneration{
	"rtx2080ti": genTuring, "rtx2080": genTuring, "rtx2070": genTuring, "rtx2060": genTuring, "t4": genTuring,
	"rtx3090": genAmpere, "rtx3080": genAmpere, "rtx3070": genAmpere, "rtx3060": genAmpere,
	"a100": genAmpere, "a10": genAmpere, "a40": genAmpere, "a10g": genAmpere,
	"rtx4090": genAdaLovelace, "4090": genAdaLovelace, "rtx4080": genAdaLovelace, "4080": genAdaLovelace,
	"rtx4070": genAdaLovelace, "4070": genAdaLovelace, "rtx4060": genAdaLovelace, "4060": genAdaLovelace,
	"l40": genAdaLovelace, "l40s": genAdaLovelace, "l4": genAdaLovelace,
	"h100": genHopper, "h100sxm": genHopper, "h200": genHopper,
	"b100": genBlackwell, "b200": genBlackwell, "gb200": genBlackwell,
}

// normalizeGPU strips non-alphanumeric chars and lowercases (same logic as inventory.NormalizeGPUClass).
func normalizeGPU(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// resolveGPUFilter determines whether the GPU class constraint can be sent
// directly to Vast.ai as a gpu_name filter, or whether results need post-filtering.
// Returns:
//   - vastaiNames: specific gpu_name values for Vast.ai search (empty if post-filter needed)
//   - postFilter: function to filter offers after search (nil if not needed)
func resolveGPUFilter(gpuClass string) (vastaiNames []string, postFilter func([]Offer) []Offer) {
	if gpuClass == "" {
		return nil, nil
	}

	minMode := strings.HasSuffix(gpuClass, "+")
	base := strings.TrimSuffix(gpuClass, "+")
	norm := normalizeGPU(base)

	// Family constraint (e.g., "nvidia") — no gpu_name filter, accept all NVIDIA
	if norm == "nvidia" {
		return nil, nil // no filter needed, all Vast.ai offers are NVIDIA
	}

	// Generation name (e.g., "hopper", "hopper+")
	if gen, ok := genNames[norm]; ok {
		names := collectGenNames(gen, minMode)
		return nil, makePostFilter(names)
	}

	// Known GPU model
	if names, ok := gpuClassToVastaiNames[norm]; ok {
		if minMode {
			// e.g., "a100+" → Ampere or newer
			if gen, ok := gpuClassToGen[norm]; ok {
				allNames := collectGenNames(gen, true)
				return nil, makePostFilter(allNames)
			}
		}
		return names, nil
	}

	// Unknown — pass through as-is (will likely return no results)
	return []string{gpuClass}, nil
}

// collectGenNames collects all Vast.ai gpu_names for a generation,
// and if minMode is true, also all newer generations.
func collectGenNames(gen gpuGeneration, minMode bool) []string {
	var names []string
	for g := gen; g <= genBlackwell; g++ {
		if gNames, ok := generationGPUNames[g]; ok {
			names = append(names, gNames...)
		}
		if !minMode {
			break
		}
	}
	return names
}

// makePostFilter creates a filter function that keeps only offers whose gpu_name
// matches one of the allowed names (case-insensitive).
func makePostFilter(allowedNames []string) func([]Offer) []Offer {
	allowed := make(map[string]bool, len(allowedNames))
	for _, n := range allowedNames {
		allowed[strings.ToLower(n)] = true
	}
	return func(offers []Offer) []Offer {
		filtered := make([]Offer, 0, len(offers))
		for _, o := range offers {
			if allowed[strings.ToLower(o.GPUName)] {
				filtered = append(filtered, o)
			}
		}
		return filtered
	}
}
