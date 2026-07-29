package vastai

import "github.com/osteele/weft/internal/gpucatalog"

// defaultHeadroomGB mirrors cmd/gpu_estimator.go's defaultGPUMemHeadroomGB.
// Kept private to vastai so the filter-time effective-memory resolution
// can roll back submit-time-applied headroom when matching against a known
// hardware ceiling. Submission flow and filter flow MUST use the same value
// for the rollback math to work.
const defaultHeadroomGB = 2

// EffectiveMemGB resolves a stored gpu_mem_gb request to the value that
// should be used as a vast.ai search filter floor (`gpu_ram>=N`). The
// transformation is:
//
//  1. If (class, requested) names a known hardware ceiling exactly, return
//     requested unchanged (the user is naming the exact hardware; the
//     +2GB safety headroom would push the filter above the hardware's own
//     gpu_ram and exclude every matching offer).
//  2. If (class, requested - defaultHeadroomGB) names a known hardware
//     ceiling, return that ceiling (the requested value was persisted with
//     submit-time headroom already applied; roll it back so the filter
//     doesn't double-headroom and exclude the hardware the user asked
//     for).
//  3. Otherwise return requested + defaultHeadroomGB — the value is not a
//     recognizable hardware ceiling and is presumably a user-supplied
//     minimum; the search filter wants a 2GB cushion so we don't bid for
//     hardware with no room for the framework/runtime overhead.
//
// When class is empty or is not catalogued in gpucatalog, only step (3)
// applies. Use IntendedMemGB at consumer boundaries (reuse compatibility
// checks against a known instance capacity) where the +2GB cushion does not
// belong.
func EffectiveMemGB(class string, requested int) int {
	if requested <= 0 {
		return requested
	}
	if resolved, ok := rollbackToHardwareCeiling(class, requested); ok {
		return resolved
	}
	return requested + defaultHeadroomGB
}

// IntendedMemGB resolves the stored gpu_mem_gb to the user's original
// intent — applying only the post-headroom rollback when the stored value
// is recognizably (ceiling + defaultHeadroomGB) for a known hardware
// model. Unlike EffectiveMemGB, it does NOT add headroom for non-ceiling
// requests; the caller (e.g. reuse compatibility) is comparing against a
// known instance capacity where a 2GB cushion would falsely reject a
// rental that perfectly satisfies the request.
//
// Examples:
//
//	IntendedMemGB("a100", 82) == 80   // rollback recognized
//	IntendedMemGB("a100", 80) == 80   // exact ceiling
//	IntendedMemGB("",     24) == 24   // unknown class — passthrough, no headroom
//	IntendedMemGB("a100", 50) == 50   // not a known ceiling — passthrough
func IntendedMemGB(class string, requested int) int {
	if requested <= 0 {
		return requested
	}
	if resolved, ok := rollbackToHardwareCeiling(class, requested); ok {
		return resolved
	}
	return requested
}

// rollbackToHardwareCeiling returns (resolved, true) when (class, requested)
// is either an exact known hardware ceiling or matches (ceiling +
// defaultHeadroomGB) for one. Centralises the rollback logic so the search
// filter and the reuse check stay in lockstep.
//
// Both arms exist because the headroom can be applied at either end. Naming
// the hardware exactly must skip it ("A100 80GB" has to search `gpu_ram>=80`,
// not `>=82`, or no offer matches), and jobs persisted before that rule
// shipped carry the headroom in the stored value — wj2265 is stored as 82 and
// must resolve to the user's original 80 without a manual `weft restart`.
func rollbackToHardwareCeiling(class string, requested int) (int, bool) {
	sizes, ok := gpucatalog.MemorySizesGB(class)
	if !ok {
		return 0, false
	}
	for _, ceiling := range sizes {
		if requested == ceiling {
			return ceiling, true
		}
	}
	for _, ceiling := range sizes {
		if requested == ceiling+defaultHeadroomGB {
			return ceiling, true
		}
	}
	return 0, false
}
