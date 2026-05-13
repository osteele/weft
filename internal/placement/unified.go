package placement

import (
	"database/sql"
	"log/slog"
	"math"
	"sort"

	"github.com/osteele/weft/internal/estimate"
)

// DefaultReuseSurvival is the assumed survival probability for existing
// cloud instances (running or grace). They've already survived startup,
// so failure probability is low but nonzero.
const DefaultReuseSurvival = 0.95

// PolicyVersion stamps placement decisions for before/after retrospective
// analysis across scheduler changes.
const PolicyVersion = "weft-placement-v1"

// CandidateKind identifies the type of placement candidate.
type CandidateKind int

const (
	CandidateOnPrem     CandidateKind = iota // On-prem host
	CandidateCloudReuse                      // Existing cloud instance (grace or running)
	CandidateCloudOffer                      // New cloud instance from provider API
)

func (k CandidateKind) String() string {
	switch k {
	case CandidateOnPrem:
		return "on-prem"
	case CandidateCloudReuse:
		return "cloud-reuse"
	case CandidateCloudOffer:
		return "cloud-offer"
	default:
		return "unknown"
	}
}

// Candidate is a placement option with estimated attributes.
// The solver operates over a set of these.
type Candidate struct {
	Kind        CandidateKind
	ID          string // host name, instance ID, or offer ID
	DisplayName string

	// Estimated attributes (inputs to the solver)
	EstTime  estimate.Estimate // total completion time
	EstCost  float64           // expected $/job ($0 for on-prem)
	Survival float64           // 0-1 probability of completing (1.0 for on-prem)

	// Kind-specific detail (opaque to solver, used by caller to execute)
	OnPrem *PlacementResult  // non-nil for on-prem candidates
	Reuse  *ReuseOption      // non-nil for reuse candidates
	Offer  *CloudOfferResult // non-nil for cloud offer candidates
}

// ReuseOption describes a reusable cloud instance.
type ReuseOption struct {
	InstanceID  int64
	DisplayName string
	GPUClass    string
	GPUMemGB    int
	Status      string            // "grace" or "running"
	EstWait     estimate.Estimate // queue drain on this instance
	CostPerHour float64           // ongoing cost ($0 if grace period)
}

// CloudOfferResult describes a cloud offer for a new instance.
type CloudOfferResult struct {
	ProviderID  string
	Provider    string
	GPUName     string
	CostPerHour float64
	DLPerf      float64
	Survival    float64
	EstSetup    estimate.Estimate // launch + SSH + job setup
	EstTransfer estimate.Estimate // model/data download
	EstRun      estimate.Estimate // job execution
}

// CandidateSource collects placement candidates of a particular kind.
// Implementations are injected by callers to avoid import cycles.
type CandidateSource interface {
	Collect(db *sql.DB, constraints Constraints, predict JobPredictor) ([]Candidate, error)
}

// EvaluateRequest describes what the caller wants evaluated.
type EvaluateRequest struct {
	Constraints Constraints
	Predictor   JobPredictor
	Sources     []CandidateSource // collectors to run (nil entries skipped)
	Database    *sql.DB
}

// PlacementPlan is the output of Evaluate.
type PlacementPlan struct {
	// All evaluated candidates, sorted by EstTime ascending.
	Candidates []Candidate

	// Pre-computed picks under different objectives.
	// Nil when no candidates are feasible for that objective.
	Cheap   *Candidate // minimize expected cost
	Fast    *Candidate // minimize survival-adjusted wallclock
	Fastest *Candidate // minimize happy-path wallclock

	// True when no candidates are feasible.
	Unplaced bool
}

// Evaluate collects placement candidates from the provided sources and
// selects the best option under three objectives (cheap/fast/fastest).
// It is pure — no side effects. Callers act on the returned plan.
func Evaluate(req EvaluateRequest) (*PlacementPlan, error) {
	var candidates []Candidate

	for _, src := range req.Sources {
		if src == nil {
			continue
		}
		cs, err := src.Collect(req.Database, req.Constraints, req.Predictor)
		if err != nil {
			slog.Debug("placement source failed", "error", err)
			continue
		}
		candidates = append(candidates, cs...)
	}

	plan := &PlacementPlan{}
	if len(candidates) == 0 {
		plan.Unplaced = true
		return plan, nil
	}

	// Sort by completion time ascending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].EstTime.Mean < candidates[j].EstTime.Mean
	})
	plan.Candidates = candidates

	// Objective 1: Cheap — minimize expected cost.
	// On-prem ($0) always wins; among rentals, pick lowest cost.
	plan.Cheap = pickBest(candidates, func(c Candidate) float64 {
		return c.EstCost
	})

	// Objective 2: Fast — minimize survival-adjusted expected wallclock time.
	// E[time] = EstTime / Survival (geometric retry model).
	plan.Fast = pickBest(candidates, func(c Candidate) float64 {
		surv := c.Survival
		if surv <= 0 {
			return math.Inf(1)
		}
		return c.EstTime.Mean.Seconds() / surv
	})

	// Objective 3: Fastest — minimize happy-path wallclock time (no survival adjustment).
	plan.Fastest = pickBest(candidates, func(c Candidate) float64 {
		return c.EstTime.Mean.Seconds()
	})

	return plan, nil
}

// pickBest returns the candidate with the lowest score. Returns nil if candidates is empty.
func pickBest(candidates []Candidate, score func(Candidate) float64) *Candidate {
	if len(candidates) == 0 {
		return nil
	}
	best := 0
	bestScore := score(candidates[0])
	for i := 1; i < len(candidates); i++ {
		s := score(candidates[i])
		if s < bestScore {
			bestScore = s
			best = i
		}
	}
	return &candidates[best]
}

// MakeCandidateFromOnPrem wraps a PlacementResult into a Candidate.
func MakeCandidateFromOnPrem(result *PlacementResult) Candidate {
	return Candidate{
		Kind:        CandidateOnPrem,
		ID:          result.Host,
		DisplayName: result.Host,
		EstTime:     result.CompletionEst,
		EstCost:     0,
		Survival:    1.0,
		OnPrem:      result,
	}
}
