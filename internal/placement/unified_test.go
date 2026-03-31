package placement

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/osteele/weft/internal/estimate"
)

// mockSource returns a fixed set of candidates.
type mockSource struct {
	candidates []Candidate
	err        error
}

func (m *mockSource) Collect(_ *sql.DB, _ Constraints, _ JobPredictor) ([]Candidate, error) {
	return m.candidates, m.err
}

func TestEvaluate_NoSources(t *testing.T) {
	plan, err := Evaluate(EvaluateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Unplaced {
		t.Error("expected Unplaced=true with no sources")
	}
	if plan.Cheap != nil || plan.Fast != nil || plan.Fastest != nil {
		t.Error("expected nil picks with no sources")
	}
}

func TestEvaluate_NilSourcesSkipped(t *testing.T) {
	plan, err := Evaluate(EvaluateRequest{
		Sources: []CandidateSource{nil, nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Unplaced {
		t.Error("expected Unplaced=true with nil sources")
	}
}

func TestEvaluate_SingleOnPremCandidate(t *testing.T) {
	src := &mockSource{
		candidates: []Candidate{{
			Kind:     CandidateOnPrem,
			ID:       "cool30",
			EstTime:  estimate.Constant(60 * time.Minute),
			EstCost:  0,
			Survival: 1.0,
		}},
	}

	plan, err := Evaluate(EvaluateRequest{Sources: []CandidateSource{src}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Unplaced {
		t.Error("expected Unplaced=false")
	}
	if plan.Cheap == nil || plan.Cheap.ID != "cool30" {
		t.Error("Cheap should pick cool30")
	}
	if plan.Fast == nil || plan.Fast.ID != "cool30" {
		t.Error("Fast should pick cool30")
	}
	if plan.Fastest == nil || plan.Fastest.ID != "cool30" {
		t.Error("Fastest should pick cool30")
	}
}

func TestEvaluate_CheapPrefersOnPrem(t *testing.T) {
	src := &mockSource{
		candidates: []Candidate{
			{
				Kind:     CandidateOnPrem,
				ID:       "cool30",
				EstTime:  estimate.Constant(90 * time.Minute), // slower
				EstCost:  0,                                   // free
				Survival: 1.0,
			},
			{
				Kind:     CandidateCloudReuse,
				ID:       "instance:42",
				EstTime:  estimate.Constant(30 * time.Minute), // faster
				EstCost:  1.50,                                // $1.50
				Survival: 0.9,
			},
		},
	}

	plan, err := Evaluate(EvaluateRequest{Sources: []CandidateSource{src}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Cheap == nil || plan.Cheap.ID != "cool30" {
		t.Errorf("Cheap should pick free on-prem, got %v", plan.Cheap)
	}
	if plan.Fastest == nil || plan.Fastest.ID != "instance:42" {
		t.Errorf("Fastest should pick faster cloud, got %v", plan.Fastest)
	}
}

func TestEvaluate_FastAccountsForSurvival(t *testing.T) {
	src := &mockSource{
		candidates: []Candidate{
			{
				Kind:     CandidateCloudReuse,
				ID:       "reliable",
				EstTime:  estimate.Constant(60 * time.Minute),
				EstCost:  2.0,
				Survival: 0.95,
			},
			{
				Kind:     CandidateCloudReuse,
				ID:       "unreliable",
				EstTime:  estimate.Constant(50 * time.Minute), // faster raw
				EstCost:  1.0,
				Survival: 0.3, // very unreliable
			},
		},
	}

	plan, err := Evaluate(EvaluateRequest{Sources: []CandidateSource{src}})
	if err != nil {
		t.Fatal(err)
	}
	// Fastest ignores survival → picks "unreliable" (50min)
	if plan.Fastest == nil || plan.Fastest.ID != "unreliable" {
		t.Errorf("Fastest should pick unreliable (faster raw), got %v", plan.Fastest)
	}
	// Fast adjusts for survival: reliable=60/0.95≈63min, unreliable=50/0.3≈167min
	if plan.Fast == nil || plan.Fast.ID != "reliable" {
		t.Errorf("Fast should pick reliable (better survival-adjusted), got %v", plan.Fast)
	}
}

func TestEvaluate_MultipleSources(t *testing.T) {
	onprem := &mockSource{
		candidates: []Candidate{{
			Kind:     CandidateOnPrem,
			ID:       "cool30",
			EstTime:  estimate.Constant(120 * time.Minute),
			EstCost:  0,
			Survival: 1.0,
		}},
	}
	reuse := &mockSource{
		candidates: []Candidate{{
			Kind:     CandidateCloudReuse,
			ID:       "instance:99",
			EstTime:  estimate.Constant(40 * time.Minute),
			EstCost:  0.80,
			Survival: 0.9,
		}},
	}

	plan, err := Evaluate(EvaluateRequest{Sources: []CandidateSource{onprem, reuse}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(plan.Candidates))
	}
	// Cheap: on-prem ($0)
	if plan.Cheap.ID != "cool30" {
		t.Errorf("Cheap = %s, want cool30", plan.Cheap.ID)
	}
	// Fastest: cloud reuse (40min vs 120min)
	if plan.Fastest.ID != "instance:99" {
		t.Errorf("Fastest = %s, want instance:99", plan.Fastest.ID)
	}
}

func TestEvaluate_FailingSourceSkipped(t *testing.T) {
	failing := &mockSource{err: errTestSource}
	good := &mockSource{
		candidates: []Candidate{{
			Kind:     CandidateOnPrem,
			ID:       "cool30",
			EstTime:  estimate.Constant(60 * time.Minute),
			EstCost:  0,
			Survival: 1.0,
		}},
	}

	plan, err := Evaluate(EvaluateRequest{Sources: []CandidateSource{failing, good}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Unplaced {
		t.Error("should not be unplaced — good source succeeded")
	}
	if plan.Cheap == nil || plan.Cheap.ID != "cool30" {
		t.Error("should pick cool30 from good source")
	}
}

var errTestSource = errors.New("test source failure")

func TestEvaluate_CandidatesSortedByTime(t *testing.T) {
	src := &mockSource{
		candidates: []Candidate{
			{ID: "slow", EstTime: estimate.Constant(120 * time.Minute), Survival: 1.0},
			{ID: "fast", EstTime: estimate.Constant(30 * time.Minute), Survival: 1.0},
			{ID: "mid", EstTime: estimate.Constant(60 * time.Minute), Survival: 1.0},
		},
	}

	plan, _ := Evaluate(EvaluateRequest{Sources: []CandidateSource{src}})
	if len(plan.Candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(plan.Candidates))
	}
	if plan.Candidates[0].ID != "fast" || plan.Candidates[1].ID != "mid" || plan.Candidates[2].ID != "slow" {
		t.Errorf("candidates not sorted by time: %s, %s, %s",
			plan.Candidates[0].ID, plan.Candidates[1].ID, plan.Candidates[2].ID)
	}
}

func TestEvaluate_ZeroSurvivalExcludedFromFast(t *testing.T) {
	src := &mockSource{
		candidates: []Candidate{
			{ID: "dead", EstTime: estimate.Constant(10 * time.Minute), EstCost: 0, Survival: 0},
			{ID: "alive", EstTime: estimate.Constant(60 * time.Minute), EstCost: 0, Survival: 1.0},
		},
	}

	plan, _ := Evaluate(EvaluateRequest{Sources: []CandidateSource{src}})
	// Fast should not pick the zero-survival candidate (Inf adjusted time)
	if plan.Fast == nil || plan.Fast.ID != "alive" {
		t.Errorf("Fast should pick 'alive', got %v", plan.Fast)
	}
	// Fastest ignores survival, picks "dead" (faster raw time)
	if plan.Fastest == nil || plan.Fastest.ID != "dead" {
		t.Errorf("Fastest should pick 'dead' (fastest raw), got %v", plan.Fastest)
	}
}
