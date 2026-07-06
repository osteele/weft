package vastai

import (
	"strings"

	"github.com/osteele/weft/internal/gpucatalog"
	"github.com/osteele/weft/internal/inventory"
)

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

	// Alias compatibility: Vast supply uses both 4080S and 4080 SUPER
	// spellings, while users may request either the base or super class.
	if !minMode && norm == "rtx4080" {
		return []string{"RTX 4080", "RTX 4080S", "RTX 4080 SUPER"}, nil
	}
	if !minMode && norm == "rtx4080super" {
		return []string{"RTX 4080S", "RTX 4080 SUPER"}, nil
	}
	if !minMode && norm == "t4" {
		// Vast.ai labels the T4 as "Tesla T4"; the bare "T4" the user types
		// matches no offers. Mirror the v100 alias so "--gpu t4" resolves.
		return []string{"Tesla T4"}, nil
	}
	if !minMode && norm == "v100" {
		return []string{"Tesla V100"}, nil
	}
	if !minMode && norm == "gh200" {
		return []string{"GH200", "GH200 Superchip", "Grace Hopper"}, nil
	}

	// Generation name (e.g., "hopper", "hopper+")
	if gen, ok := gpucatalog.GenerationNameToGeneration[norm]; ok {
		names := gpucatalog.NamesForGeneration(gen, minMode)
		return nil, makePostFilter(names)
	}

	matchAndApply := func(matchNorm string) ([]string, func([]Offer) []Offer, bool) {
		names := matchNormalizedVastaiNames(matchNorm)
		if len(names) == 0 {
			return nil, nil, false
		}
		if minMode {
			if gen, ok := generationFromVastaiNames(names); ok {
				return nil, makePostFilter(gpucatalog.NamesForGeneration(gen, true)), true
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
	return gpucatalog.MatchNormalizedNames(norm)
}

func generationFromVastaiNames(names []string) (gpucatalog.Generation, bool) {
	if len(names) == 0 {
		return gpucatalog.GenUnknown, false
	}
	return gpucatalog.GenerationForName(names[0])
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
