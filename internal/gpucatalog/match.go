package gpucatalog

import "strings"

// classAliases records normalized class names that denote the same part under
// different spellings. Aliasing is a property of the parts themselves, not of
// any provider or of weft's placement policy, so it belongs here rather than
// duplicated across the matchers that consume it.
var classAliases = map[string][]string{
	"a100sxm":  {"a100sxm4"},
	"h100hbm3": {"h100sxm"},
	// Providers carry NVIDIA's marketing prefix on datacenter Teslas; users
	// type the bare model. Without these, "--gpu t4" resolves to no part.
	"t4":   {"teslat4"},
	"v100": {"teslav100"},
	// Spelling inherited from the pre-merge memory table; its exact provider
	// meaning is unverified, so it aliases rather than inventing a part row.
	"rtxpro6000s": {"rtxpro6000"},
}

// classEquivalents is classAliases closed over symmetry — if a names b then b
// names a. The relation is fixed at load, so it is resolved once here rather
// than rediscovered by scanning the whole table on every match.
var classEquivalents = func() map[string][]string {
	m := make(map[string][]string, len(classAliases)*2)
	for alias, targets := range classAliases {
		for _, target := range targets {
			if target == "" || target == alias {
				continue
			}
			m[alias] = append(m[alias], target)
			m[target] = append(m[target], alias)
		}
	}
	return m
}()

// variantSuffixes are package / form-factor qualifiers that may be appended to
// a base class name. They are part of a part's identity — an H100 PCIe is not
// an H100 SXM — but a bare base class matches all of them.
var variantSuffixes = map[string]bool{
	"pcie": true, "sxm": true, "sxm2": true, "sxm4": true,
	"nvl": true, "superchip": true,
}

// NormalizedAliases returns the other normalized spellings that denote the same
// part as norm, excluding norm itself. Callers that have already tried norm
// take this rather than NormalizedEquivalents, so they do not repeat it.
func NormalizedAliases(norm string) []string { return classEquivalents[norm] }

// NormalizedEquivalents returns the normalized spellings that denote the same
// part as norm, including norm itself.
func NormalizedEquivalents(norm string) []string {
	return append([]string{norm}, classEquivalents[norm]...)
}

// MatchesPartName reports whether a normalized class constraint denotes the
// same part as a normalized candidate name, on the NAME axis only.
//
// Memory is deliberately excluded: a part name never carries a capacity —
// "A100 SXM4" names both the 40GB and the 80GB part — so a memory token in the
// constraint cannot be answered here and is stripped before comparison.
// Callers that need SKU identity must check the memory axis separately against
// the candidate's reported capacity. Keeping both axes in one predicate is
// what produced two matchers that disagreed: one stripped the token and one
// did not, so the same constraint string was enforced differently depending on
// which path evaluated it.
//
// A bare base class matches its variants ("h100" matches "h100nvl"), because a
// user who did not name a variant accepts any. A named variant does not match
// a different one.
func MatchesPartName(constraintNorm, candidateNorm string) bool {
	constraintNorm = trimNormalizedMemorySuffix(constraintNorm)
	candidateNorm = trimNormalizedMemorySuffix(candidateNorm)
	if constraintNorm == "" || candidateNorm == "" {
		return false
	}
	for _, constraint := range NormalizedEquivalents(constraintNorm) {
		for _, candidate := range NormalizedEquivalents(candidateNorm) {
			if denotesSamePart(constraint, candidate) {
				return true
			}
		}
	}
	return false
}

// denotesSamePart compares two spellings that have already been resolved
// through their aliases and stripped of any memory token.
func denotesSamePart(constraint, candidate string) bool {
	if candidate == constraint {
		return true
	}
	// A bare base class accepts any variant: the user named no preference.
	if strings.HasPrefix(candidate, constraint) && variantSuffixes[candidate[len(constraint):]] {
		return true
	}
	// A bare model number denotes its rtx-prefixed spelling.
	return strings.HasPrefix(candidate, "rtx") && strings.TrimPrefix(candidate, "rtx") == constraint
}

// trimNormalizedMemorySuffix strips a trailing memory token from an
// already-normalized class ("a100sxm480gb" -> "a100sxm4"). Unlike
// SplitTrailingMemorySuffix it works post-normalization, where the separators
// that make the token unambiguous are gone — "a100sxm480gb" could be sxm4 plus
// 80GB or sxm plus 480GB. It therefore reports only the base, and only when the
// split leaves a catalogued class behind. Callers that need the capacity must
// read it from the raw form via SplitTrailingMemorySuffix.
func trimNormalizedMemorySuffix(norm string) string {
	trimmed := trimTrailingMemorySuffix(norm)
	if trimmed == norm {
		return norm
	}
	if _, ok := normalizedToNames[trimmed]; ok {
		return trimmed
	}
	if _, ok := MemorySizesGB(trimmed); ok {
		return trimmed
	}
	return norm
}
