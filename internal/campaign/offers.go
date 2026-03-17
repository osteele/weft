package campaign

import (
	"os"
	"strconv"
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

func offerConstraintsForGroup(group InstanceGroup) cloud.OfferConstraints {
	c := cloud.OfferConstraints{
		GPUClass:       group.GPUClass,
		MinGPUMemGB:    group.GPUMemGB,
		MinDiskGB:      group.DiskGB,
		MinReliability: cloud.DefaultMinReliability,
	}
	if group.HasComputeIntensiveJob() {
		c.MinCPUCoresEffective = intFromEnvOrDefault("WEFT_COMPUTE_CPU_CORES", 16)
	}
	return c
}

func intFromEnvOrDefault(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

func offerExclusionKey(offer cloud.Offer) string {
	return string(offer.Provider) + ":" + offer.ProviderID
}

// SearchBestOfferForGroup searches cloud providers for the best offer matching a
// group's requirements, optionally excluding previously failed offer IDs.
func SearchBestOfferForGroup(
	clients []cloud.Client,
	group InstanceGroup,
	survivalModel *bidding.SurvivalModel,
	jobDurationHrs, setupOverheadHrs float64,
	excludeOfferIDs map[string]struct{},
) GroupOffer {
	result := GroupOffer{Group: group}

	offers, err := cloud.SearchAllProviders(clients, offerConstraintsForGroup(group))
	if err != nil {
		result.Err = err
		return result
	}

	if len(excludeOfferIDs) > 0 {
		filtered := offers[:0]
		for _, offer := range offers {
			if _, excluded := excludeOfferIDs[offerExclusionKey(offer)]; excluded {
				continue
			}
			filtered = append(filtered, offer)
		}
		offers = filtered
	}

	if len(offers) == 0 {
		return result
	}

	_, best := bidding.BestOffer(survivalModel, offers, jobDurationHrs, setupOverheadHrs)
	result.Offer = &best
	if survivalModel != nil {
		result.SurvivalProb = survivalModel.OfferSurvival(best)
	}
	return result
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
			results[idx] = SearchBestOfferForGroup(clients, group, survivalModel, jobDurationHrs, setupOverheadHrs, nil)
		}(i, g)
	}

	wg.Wait()
	return results
}
