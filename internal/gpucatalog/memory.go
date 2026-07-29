package gpucatalog

import (
	"slices"
	"strings"

	"github.com/osteele/weft/internal/inventory"
)

// Per-part memory capacities. This is hardware fact, not provider policy, and
// lives beside the names and generations it describes.

// hardwareMemoryByClass maps a normalized GPU class name to the canonical
// memory sizes (per GPU, in GB) that ship in that model. Keys are
// inventory.NormalizeGPUClass results for the *user-facing* class spelling
// (what reaches OfferConstraints.GPUClass), e.g. "a100", "h100", "rtx4090",
// "t4" — rather than a provider's own gpu_name normalization ("teslat4"),
// though a few of those are carried as aliases. Lookups go through
// MemorySizesGB, which also retries with an "rtx" prefix so bare workstation
// classes ("a6000", "2080ti") resolve to their "rtx…" keys.
//
// Coverage is curated, not exhaustive: entries are the parts weft has seen
// offered by supported rental providers. An uncatalogued class is not an
// error — see MemorySizesGB for what callers owe that case.
var hardwareMemoryByClass = map[string][]int{
	// Ampere
	"a100":     {40, 80},
	"a100pcie": {40, 80},
	"a100sxm":  {40, 80},
	"a100sxm4": {40, 80},
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
	"h100":     {80},
	"h100pcie": {80},
	"h100sxm":  {80},
	"h100hbm3": {80},
	"h200":     {141},
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
	"t4":        {16},
	"teslat4":   {16},
	"v100":      {16, 32},
}

// MemorySizesGB returns the canonical per-GPU memory sizes (GB) for a
// user-facing GPU class, and whether the class is catalogued at all. A
// trailing "+" (min-mode) is stripped before lookup. (nil, false) covers an
// empty class, a family ("nvidia"), a generation, and any single model the
// table does not list.
//
// The second return value is load-bearing. Callers that validate or filter on
// an exact SKU need it to distinguish "this size is wrong for this part" from
// "weft has never heard of this part" — refusing on the latter would make a
// stale catalogue look like an empty market.
func MemorySizesGB(class string) ([]int, bool) {
	norm := inventory.NormalizeGPUClass(strings.TrimSuffix(strings.TrimSpace(class), "+"))
	if norm == "" {
		return nil, false
	}
	for _, key := range rtxCandidates(norm) {
		if sizes, ok := hardwareMemoryByClass[key]; ok {
			// Clone: the table is package state, and callers sort the
			// result for display.
			return slices.Clone(sizes), true
		}
	}
	return nil, false
}

// MaxHardwareMemGB returns the largest per-GPU VRAM (GB) shipped by a named GPU
// class, and whether the class is a recognized single model. Used to clamp the
// blanket default GPU-memory reservation down to a named card's real capacity
// so a request like "--gpu t4" (16GB) doesn't inherit a 20GB default that no
// T4 offer can satisfy. Returns (0, false) for empty/family/generation classes.
func MaxHardwareMemGB(class string) (int, bool) {
	sizes, ok := MemorySizesGB(class)
	if !ok || len(sizes) == 0 {
		return 0, false
	}
	return slices.Max(sizes), true
}

// KnownHardwareMemoryGB reports whether (class, memGB) names a known
// hardware ceiling, so submission paths can skip the +2GB headroom up
// front. Submission and filter must agree on the table to keep the
// rollback math invariant.
func KnownHardwareMemoryGB(class string, memGB int) bool {
	if memGB <= 0 {
		return false
	}
	sizes, ok := MemorySizesGB(class)
	if !ok {
		return false
	}
	return slices.Contains(sizes, memGB)
}

// KnownLargeHardwareMemoryGB reports whether memGB is a catalogued per-GPU
// hardware capacity large enough that users commonly mean "pick this VRAM
// class" rather than "my workload needs exactly this many GB". This is used
// for broad selectors such as "nvidia" or "ampere+" where no single model
// table entry exists, but values like 24/32/40/48/80 still name real market
// buckets. Small cards remain workload-style requests so gpu-mem=8 keeps the
// usual safety headroom.
func KnownLargeHardwareMemoryGB(memGB int) bool {
	if memGB < 24 {
		return false
	}
	for _, sizes := range hardwareMemoryByClass {
		if slices.Contains(sizes, memGB) {
			return true
		}
	}
	return false
}

// NominalHardwareMemoryGB maps an observed per-GPU memory size to the marketing
// capacity of the SKU it belongs to: the smallest catalogued size for the class
// that is at least observedGB. Returns 0 when the class is not catalogued or
// the observation exceeds every known size for it.
//
// Drivers report *usable* memory, which sits a few percent below the nominal
// capacity after ECC and reserve — an A100 40GB reports 39GB, an RTX A5000 24GB
// reports 22GB. Comparing a raw observation against a nominal figure therefore
// reads every correctly-served card as a shortfall. Rounding up to the SKU
// first distinguishes "this is the part you asked for, reported honestly" from
// "this is a smaller part": 39GB is the 40GB SKU, not a shortfall against 40,
// but still a shortfall against 80.
func NominalHardwareMemoryGB(class string, observedGB int) int {
	if observedGB <= 0 {
		return 0
	}
	sizes, ok := MemorySizesGB(class)
	if !ok {
		return 0
	}
	best := 0
	for _, size := range sizes {
		if size >= observedGB && (best == 0 || size < best) {
			best = size
		}
	}
	return best
}

// MatchesSKUMemoryGB reports whether a device of the given class, observed to
// have observedGB, is the SKU that requestedGB names — the memory axis of
// specs/inventory-placement.allium rule MatchSKUMemory, whose three clauses
// (no token, uncatalogued class, catalogued part) this implements in order.
//
// requestedGB of 0 means the class named no capacity; observedGB of 0 means the
// device reported none. Both are unknown rather than wrong, so both pass.
//
// Every caller must come through here rather than compare sizes itself: the
// rounding and the unknown-is-not-disqualifying rule are what two hand-rolled
// copies previously disagreed about.
func MatchesSKUMemoryGB(class string, requestedGB, observedGB int) bool {
	if requestedGB <= 0 || observedGB <= 0 {
		return true
	}
	if _, known := MemorySizesGB(class); !known {
		return true
	}
	if nominal := NominalHardwareMemoryGB(class, observedGB); nominal > 0 {
		return nominal == requestedGB
	}
	return observedGB == requestedGB
}
