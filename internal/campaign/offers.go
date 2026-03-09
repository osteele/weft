package campaign

import (
	"sync"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
)

// GroupOffer pairs an instance group with its best cloud offer.
type GroupOffer struct {
	Group        InstanceGroup
	Offer        *cloud.Offer // nil if no offers found
	SurvivalProb float64      // 0 if no survival model available
	Err          error
}

// FetchGroupOffers searches cloud providers for the best offer per group, in parallel.
// When survivalModel is non-nil, selects the offer with lowest expected cost (including
// retry risk from preemption). Otherwise falls back to cheapest offer.
// jobDurationHrs and setupOverheadHrs are used for expected cost computation.
func FetchGroupOffers(clients []cloud.Client, groups []InstanceGroup, survivalModel *bidding.SurvivalModel, jobDurationHrs, setupOverheadHrs float64) []GroupOffer {
	results := make([]GroupOffer, len(groups))
	var wg sync.WaitGroup

	for i, g := range groups {
		results[i].Group = g
		wg.Add(1)
		go func(idx int, group InstanceGroup) {
			defer wg.Done()

			constraints := cloud.OfferConstraints{
				GPUClass:       group.GPUClass,
				MinGPUMemGB:    group.GPUMemGB,
				MinDiskGB:      group.DiskGB,
				MinReliability: cloud.DefaultMinReliability,
			}

			offers, err := cloud.SearchAllProviders(clients, constraints)
			if err != nil {
				results[idx].Err = err
				return
			}
			if len(offers) == 0 {
				return
			}

			_, best := bidding.BestOffer(survivalModel, offers, jobDurationHrs, setupOverheadHrs)
			results[idx].Offer = &best

			if survivalModel != nil {
				results[idx].SurvivalProb = survivalModel.OfferSurvival(best)
			}
		}(i, g)
	}

	wg.Wait()
	return results
}
