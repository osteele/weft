package placement

import (
	"database/sql"
	"errors"
)

// OnPremSource implements CandidateSource for on-prem inventory hosts.
type OnPremSource struct{}

func (s *OnPremSource) Collect(db *sql.DB, constraints Constraints, predict JobPredictor) ([]Candidate, error) {
	result, err := PlaceOnPrem(db, constraints, predict)
	if err != nil {
		if errors.Is(err, ErrNoEligibleHost) || errors.Is(err, ErrNoReachableHost) {
			return nil, nil // no candidates, not an error
		}
		return nil, err
	}
	c := MakeCandidateFromOnPrem(result)
	return []Candidate{c}, nil
}
