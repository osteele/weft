package gpucatalog

import (
	"testing"

	"github.com/osteele/weft/internal/inventory"
)

func TestCatalogEntriesHaveGenerationAndComputeCap(t *testing.T) {
	for _, entry := range Entries {
		if entry.Gen == GenUnknown {
			t.Fatalf("%s has unknown generation", entry.Name)
		}
		if entry.ComputeCap == "" {
			t.Fatalf("%s has empty compute cap", entry.Name)
		}
		if got := ComputeCapForGPU(entry.Name); got != entry.ComputeCap {
			t.Fatalf("ComputeCapForGPU(%q) = %q, want %q", entry.Name, got, entry.ComputeCap)
		}
	}
}

func TestProblemModelsClassifyConsistently(t *testing.T) {
	cases := []struct {
		name string
		gen  Generation
		cap  string
	}{
		{"RTX PRO 4500", GenBlackwell, "12.0"},
		{"RTX PRO 5000", GenBlackwell, "12.0"},
		{"RTX PRO 6000 WS", GenBlackwell, "12.0"},
		{"A100", GenAmpere, "8.0"},
		{"H100", GenHopper, "9.0"},
		{"GH200", GenHopper, "9.0"},
		{"Grace Hopper", GenHopper, "9.0"},
	}
	for _, c := range cases {
		if got := ComputeCapForGPU(c.name); got != c.cap {
			t.Errorf("ComputeCapForGPU(%q) = %q, want %q", c.name, got, c.cap)
		}
		if gen, ok := GenerationForNormalizedClass(inventory.NormalizeGPUClass(c.name)); !ok || gen != c.gen {
			t.Errorf("GenerationForNormalizedClass(%q) = %v,%v; want %v,true", c.name, gen, ok, c.gen)
		}
	}
}

// TestBlackwellComputeCaps covers the wb42 / wj2365 cards, including the
// "Blackwell"-suffixed provider name variants that Vast.ai reports (the terse
// "RTX PRO 4500" GPUName in the incident missed the generation-name fallback).
// It also pins the fail-closed contract callers depend on: an uncatalogued name
// resolves to "" so placement/reuse/offer filters reject it under an arch cap
// rather than admitting a too-new card.
func TestBlackwellComputeCaps(t *testing.T) {
	for _, name := range []string{
		"RTX PRO 6000 Blackwell",
		"RTX PRO 5000 Blackwell",
		"RTX PRO 4500 Blackwell",
		"RTX PRO 6000",
		"RTX PRO 5000",
		"RTX PRO 4500",
		"RTX 5090",
		"RTX 5080",
	} {
		if got := ComputeCapForGPU(name); got != "12.0" {
			t.Errorf("ComputeCapForGPU(%q) = %q, want %q", name, got, "12.0")
		}
	}

	for _, name := range []string{"Mystery Accelerator Z9", "Totally Unknown GPU X"} {
		if got := ComputeCapForGPU(name); got != "" {
			t.Errorf("ComputeCapForGPU(%q) = %q, want \"\" (unknown must fail closed at callers)", name, got)
		}
	}
}
