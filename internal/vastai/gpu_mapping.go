package vastai

import (
	"slices"
	"strings"

	"github.com/osteele/weft/internal/inventory"
)

type vastaiGPU struct {
	Name string
	Gen  gpuGeneration
}

// gpuGeneration represents an ordered GPU generation for Vast.ai filtering.
type gpuGeneration int

const (
	genUnknown gpuGeneration = iota
	// Pre-Volta generations: present so torch arch-min filtering can REJECT
	// these GPUs when a torch-derived min-cap is set (e.g. torch 2.10
	// requires sm_75+, so 1080 Ti at sm_61 must be filtered out — but only
	// if weft can name the generation first).
	genMaxwell
	genPascal
	genVolta
	genTuring
	genAmpere
	genAdaLovelace
	genHopper
	genBlackwell
)

var generationNameToGen = map[string]gpuGeneration{
	"maxwell":     genMaxwell,
	"pascal":      genPascal,
	"volta":       genVolta,
	"turing":      genTuring,
	"ampere":      genAmpere,
	"ada":         genAdaLovelace,
	"adalovelace": genAdaLovelace,
	"hopper":      genHopper,
	"blackwell":   genBlackwell,
}

var genToGenerationName = map[gpuGeneration]string{
	genMaxwell:     "maxwell",
	genPascal:      "pascal",
	genVolta:       "volta",
	genTuring:      "turing",
	genAmpere:      "ampere",
	genAdaLovelace: "adalovelace",
	genHopper:      "hopper",
	genBlackwell:   "blackwell",
}

// vastaiGPUCatalog is the single source of truth for Vast.ai gpu_name values.
var vastaiGPUCatalog = []vastaiGPU{
	// Maxwell (sm_52). Listed so torch-arch min filtering can reject
	// them. Tegra/Jetson variants at sm_53 are not relevant here.
	{"Tesla M40", genMaxwell},
	{"Tesla M60", genMaxwell},
	{"GTX TITAN X", genMaxwell},
	{"GTX 980 Ti", genMaxwell},
	{"GTX 980", genMaxwell},
	{"GTX 970", genMaxwell},
	{"GTX 960", genMaxwell},
	{"GTX 950", genMaxwell},

	// Pascal (sm_61, except GP100 at sm_60). Listed so torch-arch min
	// filtering can reject them — these are what triggered EXP-179
	// wj2240 landing on a GTX 1080 Ti despite torch 2.10's sm_75+ floor.
	{"Tesla P100", genPascal},
	{"Tesla P40", genPascal},
	{"Tesla P4", genPascal},
	{"Quadro GP100", genPascal},
	{"TITAN Xp", genPascal},
	{"GTX 1080 Ti", genPascal},
	{"GTX 1080", genPascal},
	{"GTX 1070 Ti", genPascal},
	{"GTX 1070", genPascal},
	{"GTX 1060", genPascal},
	{"GTX 1050 Ti", genPascal},
	{"GTX 1050", genPascal},

	// Volta
	{"Tesla V100", genVolta},
	{"V100", genVolta},

	// Turing
	{"RTX 2080 Ti", genTuring},
	{"RTX 2080", genTuring},
	{"RTX 2070", genTuring},
	{"RTX 2060", genTuring},
	{"Tesla T4", genTuring},

	// Ampere
	{"RTX 3090", genAmpere},
	{"RTX 3090 Ti", genAmpere},
	{"RTX 3080", genAmpere},
	{"RTX 3080 Ti", genAmpere},
	{"RTX 3070", genAmpere},
	{"RTX 3070 Ti", genAmpere},
	{"RTX 3060", genAmpere},
	{"RTX 3060 Ti", genAmpere},
	{"RTX A5000", genAmpere},
	{"RTX A6000", genAmpere},
	{"A100 PCIE", genAmpere},
	{"A100 SXM4", genAmpere},
	{"A100X", genAmpere},
	{"A10", genAmpere},
	{"A40", genAmpere},
	{"A10G", genAmpere},

	// Ada Lovelace
	{"RTX 4090", genAdaLovelace},
	{"RTX 4080", genAdaLovelace},
	{"RTX 4080S", genAdaLovelace},
	{"RTX 4070 Ti", genAdaLovelace},
	{"RTX 4070", genAdaLovelace},
	{"RTX 4070S Ti", genAdaLovelace},
	{"RTX 4060 Ti", genAdaLovelace},
	{"RTX 4060", genAdaLovelace},
	{"RTX 6000Ada", genAdaLovelace},
	{"RTX PRO 6000 WS", genAdaLovelace},
	{"L40", genAdaLovelace},
	{"L40S", genAdaLovelace},
	{"L20", genAdaLovelace},
	{"L4", genAdaLovelace},

	// Hopper
	{"H100 SXM", genHopper},
	{"H100 NVL", genHopper},
	{"H100 PCIE", genHopper},
	{"H200", genHopper},
	{"H200 NVL", genHopper},

	// Blackwell
	{"RTX 5090", genBlackwell},
	{"RTX 5080", genBlackwell},
	{"RTX 5070 Ti", genBlackwell},
	{"RTX 5070", genBlackwell},
	{"RTX 5060 Ti", genBlackwell},
	{"RTX PRO 5000", genBlackwell},
	{"B100", genBlackwell},
	{"B200", genBlackwell},
	{"B200 NVL", genBlackwell},
	{"GB200", genBlackwell},
}

var (
	normalizedToVastai    map[string][]string
	vastaiToGen           map[string]gpuGeneration
	genToVastaiNames      map[gpuGeneration][]string
	normalizedClassToGen  map[string]gpuGeneration
	normalizedClassLookup []string
)

func init() {
	normalizedToVastai = make(map[string][]string, len(vastaiGPUCatalog))
	vastaiToGen = make(map[string]gpuGeneration, len(vastaiGPUCatalog))
	genToVastaiNames = make(map[gpuGeneration][]string, len(genToGenerationName))
	normalizedClassToGen = make(map[string]gpuGeneration, len(vastaiGPUCatalog))
	seenClass := make(map[string]bool, len(vastaiGPUCatalog))

	for _, entry := range vastaiGPUCatalog {
		norm := inventory.NormalizeGPUClass(entry.Name)
		normalizedToVastai[norm] = append(normalizedToVastai[norm], entry.Name)
		vastaiToGen[entry.Name] = entry.Gen
		genToVastaiNames[entry.Gen] = append(genToVastaiNames[entry.Gen], entry.Name)
		normalizedClassToGen[norm] = entry.Gen
		if !seenClass[norm] {
			normalizedClassLookup = append(normalizedClassLookup, norm)
			seenClass[norm] = true
		}
	}
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
	norm := inventory.NormalizeGPUClass(base)

	// Family constraint (e.g., "nvidia") — no gpu_name filter, accept all NVIDIA
	if norm == "nvidia" {
		return nil, nil // no filter needed, all Vast.ai offers are NVIDIA
	}

	// Alias compatibility: treat "rtx-4080" as matching both 4080 and 4080S
	// because Vast supply often labels equivalent Ada stock as 4080S.
	if !minMode && norm == "rtx4080" {
		return []string{"RTX 4080", "RTX 4080S"}, nil
	}
	if !minMode && norm == "v100" {
		return []string{"Tesla V100"}, nil
	}

	// Generation name (e.g., "hopper", "hopper+")
	if gen, ok := generationNameToGen[norm]; ok {
		names := collectGenNames(gen, minMode)
		return nil, makePostFilter(names)
	}

	matchAndApply := func(matchNorm string) ([]string, func([]Offer) []Offer, bool) {
		names := matchNormalizedVastaiNames(matchNorm)
		if len(names) == 0 {
			return nil, nil, false
		}
		if minMode {
			if gen, ok := generationFromVastaiNames(names); ok {
				return nil, makePostFilter(collectGenNames(gen, true)), true
			}
		}
		return names, nil, true
	}

	// Exact/prefix normalized match.
	if names, postFilter, ok := matchAndApply(norm); ok {
		return names, postFilter
	}

	// Retry with an RTX prefix when no direct match exists.
	if !strings.HasPrefix(norm, "rtx") {
		if names, postFilter, ok := matchAndApply("rtx" + norm); ok {
			return names, postFilter
		}
	}

	// Unknown — pass through as-is (will likely return no results)
	return []string{gpuClass}, nil
}

func matchNormalizedVastaiNames(norm string) []string {
	if names := normalizedToVastai[norm]; len(names) > 0 {
		return slices.Clone(names)
	}
	if trimmed := trimTrailingMemorySuffix(norm); trimmed != norm {
		if names := normalizedToVastai[trimmed]; len(names) > 0 {
			return slices.Clone(names)
		}
		norm = trimmed
	}
	if !containsDigit(norm) {
		return nil
	}
	filtered := make([]string, 0)
	for _, entry := range vastaiGPUCatalog {
		if strings.HasPrefix(inventory.NormalizeGPUClass(entry.Name), norm) {
			filtered = append(filtered, entry.Name)
		}
	}
	return filtered
}

func trimTrailingMemorySuffix(norm string) string {
	if !strings.HasSuffix(norm, "gb") {
		return norm
	}
	i := len(norm) - 3 // char before "gb"
	for i >= 0 && norm[i] >= '0' && norm[i] <= '9' {
		i--
	}
	// Only trim when there was at least one trailing digit (e.g. "80gb").
	if i == len(norm)-3 {
		return norm
	}
	return norm[:i+1]
}

func containsDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func generationFromVastaiNames(names []string) (gpuGeneration, bool) {
	if len(names) == 0 {
		return genUnknown, false
	}
	gen, ok := vastaiToGen[names[0]]
	if !ok {
		return genUnknown, false
	}
	return gen, true
}

// NVIDIAGenerationNameForNormalizedGPUClass returns the NVIDIA generation name
// for a normalized GPU class token (e.g. "a5000", "rtx4090", "h100sxm4").
func NVIDIAGenerationNameForNormalizedGPUClass(normalizedClass string) (string, bool) {
	for _, key := range []string{normalizedClass, "rtx" + normalizedClass} {
		if gen, ok := normalizedClassToGen[key]; ok {
			return genToGenerationName[gen], true
		}
		if names := matchNormalizedVastaiNames(key); len(names) > 0 {
			if gen, ok := generationFromVastaiNames(names); ok {
				return genToGenerationName[gen], true
			}
		}
		if strings.HasPrefix(key, "rtx") {
			break
		}
	}
	return "", false
}

// KnownNormalizedGPUClasses returns normalized NVIDIA class tokens derived from
// the Vast.ai GPU catalog. Callers can use these for full-name substring probes.
func KnownNormalizedGPUClasses() []string {
	return slices.Clone(normalizedClassLookup)
}

// collectGenNames collects all Vast.ai gpu_names for a generation,
// and if minMode is true, also all newer generations.
func collectGenNames(gen gpuGeneration, minMode bool) []string {
	var names []string
	for g := gen; g <= genBlackwell; g++ {
		if gNames, ok := genToVastaiNames[g]; ok {
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

// GPUClassMatchesOfferName reports whether a --gpu-class constraint can match a
// specific Vast.ai offer gpu_name under the same mapping logic used for
// provider-side search filtering.
func GPUClassMatchesOfferName(gpuClass, gpuName string) bool {
	if strings.TrimSpace(gpuClass) == "" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(gpuClass), strings.TrimSpace(gpuName)) {
		return true
	}

	names, postFilter := resolveGPUFilter(gpuClass)
	if len(names) > 0 {
		for _, name := range names {
			if strings.EqualFold(name, gpuName) {
				return true
			}
		}
		return false
	}
	if postFilter != nil {
		return len(postFilter([]Offer{{GPUName: gpuName}})) > 0
	}
	return true
}
