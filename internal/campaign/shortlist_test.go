package campaign

import (
	"fmt"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestShortlistRawOffers_KeepsCheapAndFastExtremes(t *testing.T) {
	raw := []GroupRawOffers{{
		Group: InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 24},
		Offers: []cloud.Offer{
			{ProviderID: "cheap", GPUName: "RTX 3090", CostPerHour: 0.10, DLPerf: 10},
			{ProviderID: "balanced", GPUName: "RTX 4090", CostPerHour: 0.40, DLPerf: 30},
			{ProviderID: "fast", GPUName: "H100", CostPerHour: 2.00, DLPerf: 60},
		},
	}}

	got := ShortlistRawOffers(raw)
	if len(got) != 1 {
		t.Fatalf("group count = %d, want 1", len(got))
	}
	if len(got[0].Offers) != 3 {
		t.Fatalf("offer count = %d, want 3", len(got[0].Offers))
	}
	seen := map[string]bool{}
	for _, offer := range got[0].Offers {
		seen[offer.ProviderID] = true
	}
	for _, providerID := range []string{"cheap", "fast"} {
		if !seen[providerID] {
			t.Fatalf("shortlist missing %q in %#v", providerID, got[0].Offers)
		}
	}
}

func TestShortlistRawOffers_LimitsLargeOfferSets(t *testing.T) {
	offers := make([]cloud.Offer, 0, DefaultShortlistPerGroup+8)
	for i := 0; i < DefaultShortlistPerGroup+8; i++ {
		offers = append(offers, cloud.Offer{
			ProviderID:  fmt.Sprintf("offer-%d", i),
			GPUName:     fmt.Sprintf("GPU-%d", i),
			CostPerHour: float64(i + 1),
			DLPerf:      float64((i % 5) + 1),
		})
	}

	got := ShortlistRawOffers([]GroupRawOffers{{
		Group:  InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 24},
		Offers: offers,
	}})
	if len(got[0].Offers) != DefaultShortlistPerGroup {
		t.Fatalf("offer count = %d, want %d", len(got[0].Offers), DefaultShortlistPerGroup)
	}
}
