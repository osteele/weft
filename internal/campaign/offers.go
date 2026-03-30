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
	Group          InstanceGroup
	Offer          *cloud.Offer // nil if no offers found
	SurvivalProb   float64      // 0 if no survival model available
	RejectedGroups []bidding.RejectedGroup
	Err            error
}

func offerConstraintsForGroup(group InstanceGroup) cloud.OfferConstraints {
	c := cloud.OfferConstraints{
		GPUClass:       group.GPUClass,
		MinGPUMemGB:    group.GPUMemGB,
		MaxGPUMemGB:    group.MaxGPUMemGB,
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

// rankOffer selects the best offer from a slice and returns a GroupOffer.
func rankOffer(group InstanceGroup, offers []cloud.Offer, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupOverhead bidding.OfferSetupFunc, strategy bidding.SelectionStrategy, minSurvival float64) GroupOffer {
	result := GroupOffer{Group: group}
	if len(offers) == 0 {
		return result
	}
	filtered, rejected := bidding.FilterOffersBySurvival(survivalModel, offers, minSurvival)
	result.RejectedGroups = rejected
	if len(filtered) == 0 {
		return result
	}
	_, best := bidding.BestOffer(survivalModel, filtered, jobDurationHrs, setupOverhead, strategy)
	result.Offer = &best
	if survivalModel != nil {
		result.SurvivalProb = survivalModel.OfferSurvival(best)
	}
	return result
}

// SearchBestOfferForGroup searches cloud providers for the best offer matching a
// group's requirements, optionally excluding previously failed offer IDs.
func SearchBestOfferForGroup(
	clients []cloud.Client,
	group InstanceGroup,
	survivalModel *bidding.SurvivalModel,
	jobDurationHrs float64,
	setupOverhead bidding.OfferSetupFunc,
	excludeOfferIDs map[string]struct{},
	strategy bidding.SelectionStrategy,
	minSurvival float64,
) GroupOffer {
	offers, err := cloud.SearchAllProviders(clients, offerConstraintsForGroup(group))
	if err != nil {
		return GroupOffer{Group: group, Err: err}
	}

	if len(excludeOfferIDs) > 0 {
		filtered := offers[:0]
		for _, offer := range offers {
			if _, excluded := excludeOfferIDs[offer.Key()]; excluded {
				continue
			}
			filtered = append(filtered, offer)
		}
		offers = filtered
	}

	return rankOffer(group, offers, survivalModel, jobDurationHrs, setupOverhead, strategy, minSurvival)
}

// GroupRawOffers pairs an instance group with all available cloud offers (unranked).
type GroupRawOffers struct {
	Group  InstanceGroup
	Offers []cloud.Offer
	Err    error
}

// FetchGroupRawOffers searches cloud providers for all offers per group, in parallel.
// Returns unranked offers suitable for caching and later ranking by strategy.
func FetchGroupRawOffers(clients []cloud.Client, groups []InstanceGroup) []GroupRawOffers {
	results := make([]GroupRawOffers, len(groups))
	var wg sync.WaitGroup

	for i, g := range groups {
		results[i].Group = g
		wg.Add(1)
		go func(idx int, group InstanceGroup) {
			defer wg.Done()
			offers, err := cloud.SearchAllProviders(clients, offerConstraintsForGroup(group))
			results[idx] = GroupRawOffers{Group: group, Offers: offers, Err: err}
		}(i, g)
	}

	wg.Wait()
	return results
}

// SetupOverheadFactory builds per-group OfferSetupFuncs. If nil, a constant
// 0.5h fallback is used for all groups.
type SetupOverheadFactory func(group InstanceGroup) bidding.OfferSetupFunc

// RankGroupOffers selects the best offer per group from cached raw offers using
// the given strategy. The setupFactory creates per-group setup overhead
// functions that account for datacenter download speed and group-specific
// download sizes. If nil, a constant 0.5h fallback is used.
func RankGroupOffers(raw []GroupRawOffers, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minSurvival float64) []GroupOffer {
	results := make([]GroupOffer, len(raw))
	for i, r := range raw {
		if r.Err != nil {
			results[i] = GroupOffer{Group: r.Group, Err: r.Err}
			continue
		}
		setupOverhead := bidding.ConstantSetup(0.5)
		if setupFactory != nil {
			setupOverhead = setupFactory(r.Group)
		}
		results[i] = rankOffer(r.Group, r.Offers, survivalModel, jobDurationHrs, setupOverhead, strategy, minSurvival)
	}
	return results
}

// MedianDLPerfFromRawOffers computes the median DLPerf across all raw offers.
// Returns 0 if no offers have DLPerf data.
func MedianDLPerfFromRawOffers(rawOffers []GroupRawOffers) float64 {
	var all []cloud.Offer
	for _, r := range rawOffers {
		all = append(all, r.Offers...)
	}
	return medianDLPerf(all)
}

// MedianDLPerfFromGroupOffers computes the median DLPerf from ranked group offers.
// Returns 0 if no selected offers have DLPerf data.
func MedianDLPerfFromGroupOffers(offers []GroupOffer) float64 {
	var all []cloud.Offer
	for _, o := range offers {
		if o.Offer != nil {
			all = append(all, *o.Offer)
		}
	}
	return medianDLPerf(all)
}

func medianDLPerf(offers []cloud.Offer) float64 {
	hasPerf := false
	for _, o := range offers {
		if o.DLPerf > 0 {
			hasPerf = true
			break
		}
	}
	if !hasPerf {
		return 0
	}
	return bidding.MedianOfferDLPerf(offers)
}

// FetchGroupOffers searches cloud providers for the best offer per group, in parallel.
// When survivalModel is non-nil, selects the offer with lowest expected cost (including
// retry risk from instance failure). Otherwise falls back to cheapest offer.
func FetchGroupOffers(clients []cloud.Client, groups []InstanceGroup, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minSurvival float64) []GroupOffer {
	raw := FetchGroupRawOffers(clients, groups)
	return RankGroupOffers(raw, survivalModel, jobDurationHrs, setupFactory, strategy, minSurvival)
}
