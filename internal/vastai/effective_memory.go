package vastai

import "github.com/osteele/weft/internal/gpucatalog"

// defaultHeadroomGB mirrors cmd/gpu_estimator.go's defaultGPUMemHeadroomGB.
// It is used only to recognize historical rows that persisted headroom above
// a known hardware ceiling. Current jobs.gpu_mem_gb values are already
// effective placement floors and must not receive headroom again here.
const defaultHeadroomGB = 2

// StoredMemFloorGB resolves a stored gpu_mem_gb value to the placement floor
// consumers should enforce. Submission has already applied any requested
// safety headroom, so non-ceiling values pass through unchanged. The only
// transformation is the historical rollback for a stored value recognizable
// as (hardware ceiling + defaultHeadroomGB).
//
// Examples:
//
//	StoredMemFloorGB("a100", 82) == 80 // rollback recognized
//	StoredMemFloorGB("a100", 80) == 80 // exact ceiling
//	StoredMemFloorGB("l4", 22) == 22   // effective floor, no second headroom
//	StoredMemFloorGB("", 24) == 24     // unknown class, passthrough
func StoredMemFloorGB(class string, requested int) int {
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
