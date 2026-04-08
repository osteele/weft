package placement

import (
	"database/sql"
	"errors"
)

// OnPremSource implements CandidateSource for on-prem inventory hosts.
// When Metrics is set, those pre-collected metrics are used instead of
// probing hosts via SSH, avoiding redundant network calls when placing
// multiple jobs in a single cycle.
type OnPremSource struct {
	Metrics map[string]*HostMetrics
}

func (s *OnPremSource) Collect(db *sql.DB, constraints Constraints, predict JobPredictor) ([]Candidate, error) {
	result, err := placeOnPremWithMetrics(db, constraints, predict, s.Metrics)
	if err != nil {
		if errors.Is(err, ErrNoEligibleHost) || errors.Is(err, ErrNoReachableHost) {
			return nil, nil // no candidates, not an error
		}
		return nil, err
	}
	c := MakeCandidateFromOnPrem(result)
	return []Candidate{c}, nil
}
