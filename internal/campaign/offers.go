package campaign

import (
	"sync"

	"github.com/osteele/weft/internal/cloud"
)

// GroupOffer pairs an instance group with its best cloud offer.
type GroupOffer struct {
	Group InstanceGroup
	Offer *cloud.Offer // nil if no offers found
	Err   error
}

// FetchGroupOffers searches cloud providers for the best (cheapest) offer per group, in parallel.
// Accepts multiple cloud clients and merges offers across all providers.
// Returns results in the same order as the input groups.
func FetchGroupOffers(clients []cloud.Client, groups []InstanceGroup) []GroupOffer {
	results := make([]GroupOffer, len(groups))
	var wg sync.WaitGroup

	for i, g := range groups {
		results[i].Group = g
		wg.Add(1)
		go func(idx int, group InstanceGroup) {
			defer wg.Done()

			constraints := cloud.OfferConstraints{
				GPUClass:    group.GPUClass,
				MinGPUMemGB: group.GPUMemGB,
			}

			offers, err := cloud.SearchAllProviders(clients, constraints)
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
func cheapestOffer(offers []cloud.Offer) cloud.Offer {
	best := offers[0]
	for _, o := range offers[1:] {
		if o.CostPerHour < best.CostPerHour {
			best = o
		}
	}
	return best
}
