package cloud

import "sync"

// SearchAllProviders queries all available providers in parallel and returns
// merged offers. Offers from providers that return errors are skipped; the
// function only returns an error if ALL providers fail.
func SearchAllProviders(clients []Client, constraints OfferConstraints) ([]Offer, error) {
	type result struct {
		offers []Offer
		err    error
	}
	results := make([]result, len(clients))
	var wg sync.WaitGroup

	for i, c := range clients {
		wg.Add(1)
		go func(idx int, client Client) {
			defer wg.Done()
			offers, err := client.SearchOffers(constraints)
			results[idx] = result{offers: offers, err: err}
		}(i, c)
	}

	wg.Wait()

	var allOffers []Offer
	var lastErr error
	successCount := 0

	for _, r := range results {
		if r.err != nil {
			lastErr = r.err
			continue
		}
		successCount++
		allOffers = append(allOffers, r.offers...)
	}

	if successCount == 0 && lastErr != nil {
		return nil, lastErr
	}

	return allOffers, nil
}
