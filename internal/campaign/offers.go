package campaign

import (
	"sort"
	"sync"

	"github.com/osteele/weft/internal/vastai"
)

// GroupOffer pairs an instance group with its best Vast.ai offer.
type GroupOffer struct {
	Group InstanceGroup
	Offer *vastai.Offer // nil if no offers found
	Err   error
}

// FetchGroupOffers searches Vast.ai for the best (cheapest) offer per group, in parallel.
// Returns results in the same order as the input groups.
func FetchGroupOffers(client vastai.VastaiClient, groups []InstanceGroup) []GroupOffer {
	results := make([]GroupOffer, len(groups))
	var wg sync.WaitGroup

	for i, g := range groups {
		results[i].Group = g
		wg.Add(1)
		go func(idx int, group InstanceGroup) {
			defer wg.Done()

			constraints := vastai.OfferConstraints{
				GPUClass:    group.GPUClass,
				MinGPUMemGB: group.GPUMemGB,
			}
			offers, err := client.SearchOffers(constraints)
			if err != nil {
				results[idx].Err = err
				return
			}
			if len(offers) == 0 {
				return
			}
			best := cheapestOffer(offers)
			results[idx].Offer = &best
		}(i, g)
	}

	wg.Wait()
	return results
}

// cheapestOffer returns the offer with the lowest cost per hour.
func cheapestOffer(offers []vastai.Offer) vastai.Offer {
	best := offers[0]
	for _, o := range offers[1:] {
		if o.CostPerHour < best.CostPerHour {
			best = o
		}
	}
	return best
}

// SortOffersByCost sorts offers by ascending cost per hour.
func SortOffersByCost(offers []vastai.Offer) {
	sort.Slice(offers, func(i, j int) bool {
		return offers[i].CostPerHour < offers[j].CostPerHour
	})
}
