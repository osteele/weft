package campaign

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

// A memory token inside a GPU class names an exact part, but provider
// gpu_name values carry no memory component — the upstream search term for
// "a100-sxm4-80gb" is only "A100 SXM4", which the 40GB part satisfies. The
// offer does report memory separately, so exactness is restored here.
// This is the mechanism behind wj5503/5504/5509 receiving 40GB cards.
func TestFilterOffersBySKUMemory(t *testing.T) {
	offers := []cloud.Offer{
		{GPUName: "A100 SXM4", GPUMemGB: 39}, // 40GB part, usable memory
		{GPUName: "A100 SXM4", GPUMemGB: 79}, // 80GB part, usable memory
		{GPUName: "A100 SXM4", GPUMemGB: 80},
	}
	group := InstanceGroup{GPUClass: "a100-sxm4-80gb"}
	kept, removed, requested := filterOffersBySKUMemory(offers, group)
	if requested != 80 {
		t.Fatalf("requested = %d, want 80", requested)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (the 40GB part)", removed)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d offers, want 2", len(kept))
	}
	for _, o := range kept {
		if o.GPUMemGB < 79 {
			t.Errorf("kept a %vGB offer against an 80GB SKU request", o.GPUMemGB)
		}
	}
}

// Without a memory token the class is a variant selector, not a SKU, so
// every memory size is admissible.
func TestFilterOffersBySKUMemory_NoTokenIsNoOp(t *testing.T) {
	offers := []cloud.Offer{
		{GPUName: "A100 SXM4", GPUMemGB: 39},
		{GPUName: "A100 SXM4", GPUMemGB: 80},
	}
	if _, removed, requested := filterOffersBySKUMemory(offers, InstanceGroup{GPUClass: "a100-sxm4"}); removed != 0 || requested != 0 {
		t.Fatalf("removed = %d, requested = %d; want 0, 0", removed, requested)
	}
}

// Weft cannot validate a SKU it has never heard of, and filtering on that
// basis would make a stale catalogue look like an empty market.
func TestFilterOffersBySKUMemory_UncataloguedClassPassesThrough(t *testing.T) {
	offers := []cloud.Offer{{GPUName: "Some Future GPU", GPUMemGB: 32}}
	if _, removed, _ := filterOffersBySKUMemory(offers, InstanceGroup{GPUClass: "some-future-gpu-96gb"}); removed != 0 {
		t.Fatalf("removed = %d, want 0 for an uncatalogued class", removed)
	}
}

// The blocker string must name this stage, or an exact-SKU request that finds
// no match reads as a generic empty market.
func TestNoOffersDetail_SKUMemoryStage(t *testing.T) {
	stats := OfferFilterStats{RawCount: 3, SKUMemoryFiltered: 3, SKUMemoryRequestedGB: 80, AfterSKUMemory: 0}
	got := stats.NoOffersDetail("")
	for _, want := range []string{"exact SKU memory", "80GB", ">=80GB"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail = %q, want it to mention %q", got, want)
		}
	}
}
