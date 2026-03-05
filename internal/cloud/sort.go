package cloud

import "sort"

// SortOffersByCost sorts offers by ascending cost per hour.
func SortOffersByCost(offers []Offer) {
	sort.Slice(offers, func(i, j int) bool {
		return offers[i].CostPerHour < offers[j].CostPerHour
	})
}
