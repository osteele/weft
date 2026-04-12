package campaign

import (
	"sort"

	"github.com/osteele/weft/internal/cloud"
)

const DefaultShortlistPerGroup = 10

func ShortlistRawOffers(raw []GroupRawOffers) []GroupRawOffers {
	if len(raw) == 0 {
		return nil
	}
	shortlisted := make([]GroupRawOffers, len(raw))
	for i, groupRaw := range raw {
		shortlisted[i] = groupRaw
		if len(groupRaw.Offers) <= DefaultShortlistPerGroup {
			shortlisted[i].Offers = append([]cloud.Offer(nil), groupRaw.Offers...)
			continue
		}
		shortlisted[i].Offers = ShortlistOffers(groupRaw.Offers, DefaultShortlistPerGroup)
	}
	return shortlisted
}

func ShortlistOffers(offers []cloud.Offer, limit int) []cloud.Offer {
	if len(offers) <= limit || limit <= 0 {
		return append([]cloud.Offer(nil), offers...)
	}

	added := make(map[string]struct{}, limit)
	selected := make([]cloud.Offer, 0, min(limit, len(offers)))
	add := func(offer cloud.Offer) {
		if len(selected) >= limit {
			return
		}
		key := offer.Key()
		if _, ok := added[key]; ok {
			return
		}
		added[key] = struct{}{}
		selected = append(selected, offer)
	}

	appendFromSorted := func(sorted []cloud.Offer, quota int) {
		for _, offer := range sorted {
			if len(selected) >= limit || quota <= 0 {
				return
			}
			before := len(selected)
			add(offer)
			if len(selected) > before {
				quota--
			}
		}
	}

	byCost := append([]cloud.Offer(nil), offers...)
	sort.Slice(byCost, func(i, j int) bool {
		if byCost[i].CostPerHour == byCost[j].CostPerHour {
			return byCost[i].DLPerf > byCost[j].DLPerf
		}
		return byCost[i].CostPerHour < byCost[j].CostPerHour
	})

	byPerf := append([]cloud.Offer(nil), offers...)
	sort.Slice(byPerf, func(i, j int) bool {
		if byPerf[i].DLPerf == byPerf[j].DLPerf {
			return byPerf[i].CostPerHour < byPerf[j].CostPerHour
		}
		return byPerf[i].DLPerf > byPerf[j].DLPerf
	})

	byPerfPerDollar := append([]cloud.Offer(nil), offers...)
	sort.Slice(byPerfPerDollar, func(i, j int) bool {
		left := OfferPerfPerDollar(byPerfPerDollar[i])
		right := OfferPerfPerDollar(byPerfPerDollar[j])
		if left == right {
			return byPerfPerDollar[i].CostPerHour < byPerfPerDollar[j].CostPerHour
		}
		return left > right
	})

	appendFromSorted(byCost, min(4, limit))
	appendFromSorted(byPerf, min(3, max(limit-len(selected), 0)))
	appendFromSorted(byPerfPerDollar, min(3, max(limit-len(selected), 0)))
	appendFromSorted(byCost, limit-len(selected))
	return selected
}

func OfferPerfPerDollar(offer cloud.Offer) float64 {
	if offer.CostPerHour <= 0 {
		if offer.DLPerf > 0 {
			return offer.DLPerf
		}
		return 0
	}
	if offer.DLPerf <= 0 {
		return 0
	}
	return offer.DLPerf / offer.CostPerHour
}
