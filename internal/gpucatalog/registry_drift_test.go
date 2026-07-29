package gpucatalog

import (
	"testing"

	"github.com/osteele/weft/internal/inventory"
)

// uncataloguedMemory lists parts whose capacities weft has not recorded.
//
// These are pre-Turing consumer and datacenter cards that no GPU marketplace
// rents for ML work. They are enumerated rather than left implicit so that
// "no capacity" is a decision someone made, not an oversight — the distinction
// that let an exact-SKU request pass unenforced for H100 NVL.
//
// This list may only shrink. Adding a part without capacities fails
// TestEveryEntryDeclaresMemory below, which is the point: a wrong capacity is
// worse than a missing one now that the SKU axis rejects mismatches, so the
// choice has to be deliberate either way.
var uncataloguedMemory = map[string]bool{
	"Tesla M40": true, "Tesla M60": true, "GTX TITAN X": true,
	"GTX 980 Ti": true, "GTX 980": true, "GTX 970": true,
	"GTX 960": true, "GTX 950": true,
	"Tesla P100": true, "Tesla P40": true, "Tesla P4": true,
	"Quadro GP100": true, "TITAN Xp": true,
	"GTX 1080 Ti": true, "GTX 1080": true, "GTX 1070 Ti": true,
	"GTX 1070": true, "GTX 1060": true, "GTX 1050 Ti": true, "GTX 1050": true,
}

// Every part either declares its capacities or is explicitly listed as
// uncatalogued. Before the registries were merged, capacities lived in a
// second table keyed by normalized class, and the two drifted: 43 catalogued
// parts had no capacity row and 9 capacity rows named no part. H100 NVL was in
// the first group, so `--gpu h100-nvl-94gb` — correct hardware, correctly
// spelled — silently enforced nothing.
func TestEveryEntryDeclaresMemory(t *testing.T) {
	for _, e := range Entries {
		if len(e.MemoryGB) > 0 {
			if uncataloguedMemory[e.Name] {
				t.Errorf("%q declares capacities and is also listed as uncatalogued; drop it from the list", e.Name)
			}
			continue
		}
		if !uncataloguedMemory[e.Name] {
			t.Errorf("%q declares no capacities and is not listed as uncatalogued.\n"+
				"Add Entry.MemoryGB, or add it to uncataloguedMemory with a reason. "+
				"A part with neither is invisible to the SKU axis.", e.Name)
		}
	}
}

// The reverse direction: nothing may claim a capacity for a part that does not
// exist. Family and alias spellings derive their capacities from the parts they
// match, so they need no row of their own.
func TestNoCapacityWithoutAPart(t *testing.T) {
	parts := map[string]bool{}
	for _, e := range Entries {
		parts[inventory.NormalizeGPUClass(e.Name)] = true
	}
	for class := range memorySizesCache {
		if parts[class] {
			continue
		}
		if _, isAlias := classAliases[class]; isAlias {
			continue
		}
		if _, known := MemorySizesGB(class); known {
			// A derived family spelling is fine as long as it resolves to real
			// parts; MemorySizesGB only caches classes that matched one.
			continue
		}
		t.Errorf("capacity cached for %q, which names no part and no alias", class)
	}
}
