package gpucatalog

import (
	"slices"
	"sort"
	"strings"

	"github.com/osteele/weft/internal/inventory"
)

// Per-part memory capacities. This is hardware fact, not provider policy, and
// lives beside the names and generations it describes.

// MemorySizesGB returns the per-GPU capacities a class can resolve to, and
// whether the class is catalogued at all.
//
// The result is derived from the parts the class matches rather than stored
// per class. A family or alias spelling ("a100", "h100hbm3") has no part row:
// its capacities are the union over the parts it names, and storing that union
// would be a second copy of a fact that then drifts from the first. Before
// this was derived, "h100" carried a hand-maintained {80} while H100 NVL —
// which "h100" legitimately matches — ships 94.
//
// A class that matches only uncatalogued parts returns (nil, false): weft has
// never heard of the capacity, which callers must distinguish from a part that
// ships in one size.
func MemorySizesGB(class string) ([]int, bool) {
	norm := inventory.NormalizeGPUClass(strings.TrimSuffix(strings.TrimSpace(class), "+"))
	if norm == "" {
		return nil, false
	}
	if sizes, ok := memorySizesCache[norm]; ok {
		return slices.Clone(sizes), true
	}
	// Bare workstation model numbers ("a6000", "2080ti") name rtx-prefixed
	// parts. Mirrors the retry in resolveGPUFilter so the two agree on what a
	// user's spelling resolves to.
	if !strings.HasPrefix(norm, "rtx") {
		if sizes, ok := memorySizesCache["rtx"+norm]; ok {
			return slices.Clone(sizes), true
		}
	}
	return nil, false
}

// memorySizesCache is the derivation, computed once. Lookup is on the hot path
// for every offer in a search.
var memorySizesCache = map[string][]int{}

func buildMemorySizesCache() {
	classes := map[string]bool{}
	for _, e := range Entries {
		classes[inventory.NormalizeGPUClass(e.Name)] = true
	}
	for _, alias := range knownClassSpellings() {
		classes[alias] = true
	}
	for class := range classes {
		seen := map[int]bool{}
		var sizes []int
		for _, e := range Entries {
			if len(e.MemoryGB) == 0 {
				continue
			}
			if !MatchesPartName(class, inventory.NormalizeGPUClass(e.Name)) {
				continue
			}
			for _, size := range e.MemoryGB {
				if !seen[size] {
					seen[size] = true
					sizes = append(sizes, size)
				}
			}
		}
		if len(sizes) == 0 {
			continue
		}
		sort.Ints(sizes)
		memorySizesCache[class] = sizes
	}
}

// knownClassSpellings enumerates user-facing class strings that name a family
// or an alias rather than a single part, so their derived capacities are
// cached alongside the parts'.
func knownClassSpellings() []string {
	spellings := []string{"a100", "h100", "h200", "b200", "gh200", "v100", "t4"}
	for alias := range classAliases {
		spellings = append(spellings, alias)
		spellings = append(spellings, classAliases[alias]...)
	}
	return spellings
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
	for _, sizes := range memorySizesCache {
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
