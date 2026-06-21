package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestFilterOffersByHostRAM(t *testing.T) {
	offers := []cloud.Offer{
		{GPUName: "below", RAMGB: 16},  // below floor -> dropped
		{GPUName: "ok", RAMGB: 64},     // meets floor -> kept
		{GPUName: "unknown", RAMGB: 0}, // unknown host RAM (e.g. RunPod) -> kept
	}
	filtered, removed := filterOffersByHostRAM(offers, InstanceGroup{CPUMemGB: 32})
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered len = %d, want 2", len(filtered))
	}
	for _, o := range filtered {
		if o.GPUName == "below" {
			t.Errorf("offer below the floor should have been dropped")
		}
	}

	if _, removed := filterOffersByHostRAM(offers, InstanceGroup{}); removed != 0 {
		t.Errorf("no floor: removed = %d, want 0", removed)
	}
}

func TestOfferConstraintsForGroup_HostRAM(t *testing.T) {
	c := offerConstraintsForGroup(InstanceGroup{CPUMemGB: 48}, 0)
	if c.MinHostRAMGB != 48 {
		t.Errorf("MinHostRAMGB = %d, want 48", c.MinHostRAMGB)
	}
}
