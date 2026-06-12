package gpucatalog

import (
	"slices"
	"sort"
	"strings"

	"github.com/osteele/weft/internal/inventory"
)

// Generation represents an ordered NVIDIA GPU architecture generation.
type Generation int

const (
	GenUnknown Generation = iota
	GenMaxwell
	GenPascal
	GenVolta
	GenTuring
	GenAmpere
	GenAdaLovelace
	GenHopper
	GenBlackwell
)

type Entry struct {
	Name       string
	Gen        Generation
	ComputeCap string
}

var GenerationNameToGeneration = map[string]Generation{
	"maxwell":     GenMaxwell,
	"pascal":      GenPascal,
	"volta":       GenVolta,
	"turing":      GenTuring,
	"ampere":      GenAmpere,
	"ada":         GenAdaLovelace,
	"adalovelace": GenAdaLovelace,
	"hopper":      GenHopper,
	"blackwell":   GenBlackwell,
}

var GenerationToName = map[Generation]string{
	GenMaxwell:     "maxwell",
	GenPascal:      "pascal",
	GenVolta:       "volta",
	GenTuring:      "turing",
	GenAmpere:      "ampere",
	GenAdaLovelace: "adalovelace",
	GenHopper:      "hopper",
	GenBlackwell:   "blackwell",
}

var DefaultComputeCapByGeneration = map[Generation]string{
	GenMaxwell:     "5.2",
	GenPascal:      "6.1",
	GenVolta:       "7.0",
	GenTuring:      "7.5",
	GenAmpere:      "8.6",
	GenAdaLovelace: "8.9",
	GenHopper:      "9.0",
	GenBlackwell:   "12.0",
}

var Entries = []Entry{
	{"Tesla M40", GenMaxwell, "5.2"},
	{"Tesla M60", GenMaxwell, "5.2"},
	{"GTX TITAN X", GenMaxwell, "5.2"},
	{"GTX 980 Ti", GenMaxwell, "5.2"},
	{"GTX 980", GenMaxwell, "5.2"},
	{"GTX 970", GenMaxwell, "5.2"},
	{"GTX 960", GenMaxwell, "5.2"},
	{"GTX 950", GenMaxwell, "5.2"},

	{"Tesla P100", GenPascal, "6.1"},
	{"Tesla P40", GenPascal, "6.1"},
	{"Tesla P4", GenPascal, "6.1"},
	{"Quadro GP100", GenPascal, "6.1"},
	{"TITAN Xp", GenPascal, "6.1"},
	{"GTX 1080 Ti", GenPascal, "6.1"},
	{"GTX 1080", GenPascal, "6.1"},
	{"GTX 1070 Ti", GenPascal, "6.1"},
	{"GTX 1070", GenPascal, "6.1"},
	{"GTX 1060", GenPascal, "6.1"},
	{"GTX 1050 Ti", GenPascal, "6.1"},
	{"GTX 1050", GenPascal, "6.1"},

	{"Tesla V100", GenVolta, "7.0"},
	{"V100", GenVolta, "7.0"},

	{"RTX 2080 Ti", GenTuring, "7.5"},
	{"RTX 2080", GenTuring, "7.5"},
	{"RTX 2070", GenTuring, "7.5"},
	{"RTX 2060", GenTuring, "7.5"},
	{"Tesla T4", GenTuring, "7.5"},

	{"RTX 3090", GenAmpere, "8.6"},
	{"RTX 3090 Ti", GenAmpere, "8.6"},
	{"RTX 3080", GenAmpere, "8.6"},
	{"RTX 3080 Ti", GenAmpere, "8.6"},
	{"RTX 3070", GenAmpere, "8.6"},
	{"RTX 3070 Ti", GenAmpere, "8.6"},
	{"RTX 3060", GenAmpere, "8.6"},
	{"RTX 3060 Ti", GenAmpere, "8.6"},
	{"RTX A5000", GenAmpere, "8.6"},
	{"RTX A6000", GenAmpere, "8.6"},
	{"A100 PCIE", GenAmpere, "8.0"},
	{"A100 SXM4", GenAmpere, "8.0"},
	{"A100X", GenAmpere, "8.0"},
	{"A10", GenAmpere, "8.6"},
	{"A40", GenAmpere, "8.6"},
	{"A10G", GenAmpere, "8.6"},

	{"RTX 4090", GenAdaLovelace, "8.9"},
	{"RTX 4080", GenAdaLovelace, "8.9"},
	{"RTX 4080S", GenAdaLovelace, "8.9"},
	{"RTX 4070 Ti", GenAdaLovelace, "8.9"},
	{"RTX 4070", GenAdaLovelace, "8.9"},
	{"RTX 4070S Ti", GenAdaLovelace, "8.9"},
	{"RTX 4060 Ti", GenAdaLovelace, "8.9"},
	{"RTX 4060", GenAdaLovelace, "8.9"},
	{"RTX 6000Ada", GenAdaLovelace, "8.9"},
	{"L40", GenAdaLovelace, "8.9"},
	{"L40S", GenAdaLovelace, "8.9"},
	{"L20", GenAdaLovelace, "8.9"},
	{"L4", GenAdaLovelace, "8.9"},

	{"H100 SXM", GenHopper, "9.0"},
	{"H100 NVL", GenHopper, "9.0"},
	{"H100 PCIE", GenHopper, "9.0"},
	{"H200", GenHopper, "9.0"},
	{"H200 NVL", GenHopper, "9.0"},
	{"GH200", GenHopper, "9.0"},
	{"GH200 Superchip", GenHopper, "9.0"},
	{"Grace Hopper", GenHopper, "9.0"},

	{"RTX 5090", GenBlackwell, "12.0"},
	{"RTX 5080", GenBlackwell, "12.0"},
	{"RTX 5070 Ti", GenBlackwell, "12.0"},
	{"RTX 5070", GenBlackwell, "12.0"},
	{"RTX 5060 Ti", GenBlackwell, "12.0"},
	{"RTX PRO 4500", GenBlackwell, "12.0"},
	{"RTX PRO 5000", GenBlackwell, "12.0"},
	{"RTX PRO 6000", GenBlackwell, "12.0"},
	{"RTX PRO 6000 WS", GenBlackwell, "12.0"},
	{"B100", GenBlackwell, "10.0"},
	{"B200", GenBlackwell, "10.0"},
	{"B200 NVL", GenBlackwell, "10.0"},
	{"GB200", GenBlackwell, "10.0"},
}

var computeCapAliases = map[string]string{
	"a100":  "8.0",
	"a30":   "8.0",
	"h100":  "9.0",
	"h200":  "9.0",
	"gh200": "9.0",
	"b300":  "10.0",
}

var (
	normalizedToNames     map[string][]string
	normalizedToEntry     map[string]Entry
	namesByGeneration     map[Generation][]string
	normalizedClassLookup []string
	computeCapLookupKeys  []string
	generationNameKeys    []string
)

func init() {
	normalizedToNames = make(map[string][]string, len(Entries))
	normalizedToEntry = make(map[string]Entry, len(Entries))
	namesByGeneration = make(map[Generation][]string, len(GenerationToName))
	seenClass := make(map[string]bool, len(Entries))

	for _, entry := range Entries {
		norm := inventory.NormalizeGPUClass(entry.Name)
		normalizedToNames[norm] = append(normalizedToNames[norm], entry.Name)
		normalizedToEntry[norm] = entry
		namesByGeneration[entry.Gen] = append(namesByGeneration[entry.Gen], entry.Name)
		if !seenClass[norm] {
			normalizedClassLookup = append(normalizedClassLookup, norm)
			seenClass[norm] = true
		}
	}
	computeCapLookupKeys = make([]string, 0, len(normalizedClassLookup)+len(computeCapAliases))
	computeCapLookupKeys = append(computeCapLookupKeys, normalizedClassLookup...)
	for alias := range computeCapAliases {
		computeCapLookupKeys = append(computeCapLookupKeys, alias)
	}
	sort.Slice(computeCapLookupKeys, func(i, j int) bool {
		if len(computeCapLookupKeys[i]) != len(computeCapLookupKeys[j]) {
			return len(computeCapLookupKeys[i]) > len(computeCapLookupKeys[j])
		}
		return computeCapLookupKeys[i] < computeCapLookupKeys[j]
	})
	generationNameKeys = sortedByLenDesc(GenerationNameToGeneration)
}

func KnownNormalizedClasses() []string {
	return slices.Clone(normalizedClassLookup)
}

func NamesForNormalizedClass(norm string) []string {
	if names := normalizedToNames[norm]; len(names) > 0 {
		return slices.Clone(names)
	}
	return nil
}

func NamesForGeneration(gen Generation, minMode bool) []string {
	var names []string
	for g := gen; g <= GenBlackwell; g++ {
		if gNames, ok := namesByGeneration[g]; ok {
			names = append(names, gNames...)
		}
		if !minMode {
			break
		}
	}
	return names
}

func GenerationForNormalizedClass(normalizedClass string) (Generation, bool) {
	for _, key := range []string{normalizedClass, "rtx" + normalizedClass} {
		if entry, ok := normalizedToEntry[key]; ok {
			return entry.Gen, true
		}
		if names := MatchNormalizedNames(key); len(names) > 0 {
			if gen, ok := GenerationForName(names[0]); ok {
				return gen, true
			}
		}
		if strings.HasPrefix(key, "rtx") {
			break
		}
	}
	return GenUnknown, false
}

func GenerationNameForNormalizedClass(normalizedClass string) (string, bool) {
	gen, ok := GenerationForNormalizedClass(normalizedClass)
	if !ok {
		return "", false
	}
	name, ok := GenerationToName[gen]
	return name, ok
}

func GenerationForName(name string) (Generation, bool) {
	entry, ok := normalizedToEntry[inventory.NormalizeGPUClass(name)]
	return entry.Gen, ok
}

func MatchNormalizedNames(norm string) []string {
	if names := NamesForNormalizedClass(norm); len(names) > 0 {
		return names
	}
	if trimmed := trimTrailingMemorySuffix(norm); trimmed != norm {
		if names := NamesForNormalizedClass(trimmed); len(names) > 0 {
			return names
		}
		norm = trimmed
	}
	if !containsDigit(norm) {
		return nil
	}
	filtered := make([]string, 0)
	for _, entry := range Entries {
		if strings.HasPrefix(inventory.NormalizeGPUClass(entry.Name), norm) {
			filtered = append(filtered, entry.Name)
		}
	}
	return filtered
}

func ComputeCapForGPU(gpuName string) string {
	norm := inventory.NormalizeGPUClass(gpuName)
	if entry, ok := normalizedToEntry[norm]; ok {
		return entry.ComputeCap
	}
	if cap, ok := computeCapAliases[norm]; ok {
		return cap
	}
	for _, fragment := range computeCapLookupKeys {
		if strings.Contains(norm, fragment) {
			if entry, ok := normalizedToEntry[fragment]; ok {
				return entry.ComputeCap
			}
			return computeCapAliases[fragment]
		}
	}
	for _, genName := range generationNameKeys {
		if strings.Contains(norm, genName) {
			return DefaultComputeCapByGeneration[GenerationNameToGeneration[genName]]
		}
	}
	return ""
}

func GenerationDefaultComputeCap(gen Generation) string {
	return DefaultComputeCapByGeneration[gen]
}

func GenerationNamesSorted() []string {
	return slices.Clone(generationNameKeys)
}

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

func trimTrailingMemorySuffix(norm string) string {
	if !strings.HasSuffix(norm, "gb") {
		return norm
	}
	i := len(norm) - 3
	for i >= 0 && norm[i] >= '0' && norm[i] <= '9' {
		i--
	}
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
