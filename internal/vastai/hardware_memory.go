package vastai

import (
	"sort"

	"github.com/osteele/weft/internal/inventory"
)

// defaultHeadroomGB mirrors cmd/gpu_estimator.go's defaultGPUMemHeadroomGB.
// Kept private to vastai so the filter-time effective-memory resolution
// can roll back submit-time-applied headroom when matching against a known
// hardware ceiling. Submission flow and filter flow MUST use the same value
// for the rollback math to work.
const defaultHeadroomGB = 2

// hardwareMemoryByClass maps a normalized GPU class name to the canonical
// memory sizes (per GPU, in GB) that ship in that model. Keys are
// inventory.NormalizeGPUClass results — e.g. "a100", "h100", "rtx4090".
//
// The table is used to:
//  1. Detect when a user's requested memory matches the hardware exactly,
//     so the submission-time +2GB safety headroom is skipped (a request for
//     "A100 80GB" must search `gpu_ram>=80`, not `>=82`, or no offer matches).
//  2. Roll back the submit-time headroom for jobs persisted before this
//     change shipped, so wj2265 (stored as 82) is treated as the user's
//     original intent of 80 without a manual `weft restart`.
//
// Add entries as needed; an unknown class simply falls through and the
// headroom is applied as before. Use only models actually offered on Vast.ai.
var hardwareMemoryByClass = map[string][]int{
	// Ampere
	"a100":     {40, 80},
	"a800":     {80},
	"a40":      {48},
	"a10":      {24},
	"a10g":     {24},
	"rtxa6000": {48},
	"rtxa5000": {24},
	"rtxa4000": {16},
	"rtx3090":  {24},
	"rtx3080":  {10, 12},
	"rtx3070":  {8},
	"rtx3060":  {8, 12},
	// Hopper
	"h100": {80},
	"h200": {141},
	// Ada Lovelace
	"rtx4090":     {24},
	"rtx4080":     {16},
	"rtx4070":     {12},
	"rtx4060":     {8, 16},
	"rtx6000ada":  {48},
	"rtxpro6000s": {96},
	"rtxpro6000":  {96},
	"l40":         {48},
	"l40s":        {48},
	"l20":         {48},
	"l4":          {24},
	// Blackwell
	"rtx5090":    {32},
	"rtx5080":    {16},
	"rtx5070":    {12},
	"rtx5060":    {8, 16},
	"b100":       {96},
	"b200":       {180, 192},
	"gb200":      {192},
	"rtxpro5000": {48},
	// Turing / older
	"rtx2080ti": {11},
	"rtx2080":   {8},
	"rtx2070":   {8},
	"teslat4":   {16},
	"v100":      {16, 32},
}

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
// When class is empty or has no entry in hardwareMemoryByClass, only step
// (3) applies. Use IntendedMemGB at consumer boundaries (reuse
// compatibility checks against a known instance capacity) where the +2GB
// cushion does not belong.
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
func rollbackToHardwareCeiling(class string, requested int) (int, bool) {
	norm := inventory.NormalizeGPUClass(class)
	sizes, ok := hardwareMemoryByClass[norm]
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

// KnownHardwareMemoryGB reports whether (class, memGB) names a known
// hardware ceiling, so submission paths can skip the +2GB headroom up
// front. Submission and filter must agree on the table to keep the
// rollback math invariant.
func KnownHardwareMemoryGB(class string, memGB int) bool {
	if memGB <= 0 {
		return false
	}
	sizes, ok := hardwareMemoryByClass[inventory.NormalizeGPUClass(class)]
	if !ok {
		return false
	}
	for _, ceiling := range sizes {
		if memGB == ceiling {
			return true
		}
	}
	return false
}

// hardwareMemoryClassesSorted is a deterministic snapshot of the
// hardwareMemoryByClass keys for tests and diagnostics.
func hardwareMemoryClassesSorted() []string {
	out := make([]string, 0, len(hardwareMemoryByClass))
	for k := range hardwareMemoryByClass {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
