package status

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
)

// TestStateMachine_RandomTransitionSequences exercises ValidateTransition with
// randomly generated command sequences. It is a cheap, pure-domain state-machine
// test: no database, no goroutines, no external state. The model tracks the
// current status and last-synced status; after each command it checks that the
// implementation accepts exactly the transitions the spec table allows and that
// the UpdatesSynced side effect is applied correctly.
//
// The test logs the random seed on failure; set WEFT_TEST_SEED to reproduce.
func TestStateMachine_RandomTransitionSequences(t *testing.T) {
	seed := testSeed()
	rng := rand.New(rand.NewPCG(seed, 0))
	t.Logf("state-machine seed: %d (set WEFT_TEST_SEED to reproduce)", seed)

	iterations := 200
	maxSteps := 50
	statuses := AllStatuses()

	for i := 0; i < iterations; i++ {
		m := &smModel{
			status:       statuses[rng.IntN(len(statuses))],
			syncedStatus: "",
		}
		// Treat the initial status as already synced; from here on, only
		// transitions whose rule sets UpdatesSynced should advance it.
		m.syncedStatus = m.status

		steps := rng.IntN(maxSteps) + 1
		trace := make([]string, 0, steps)

		for step := 0; step < steps; step++ {
			cmd := randomCommand(rng, m.status)
			desc := fmt.Sprintf("%s->%s(auth=%v)", m.status, cmd.to, cmd.authoritative)
			trace = append(trace, desc)

			expectedRule, expectOK := expectedOutcome(m.status, cmd.to, cmd.authoritative)
			rule, err := ValidateTransition(m.status, cmd.to, cmd.authoritative)

			if expectOK {
				if err != nil {
					t.Fatalf("iteration %d step %d: expected success for %s, got error: %v\nseed=%d trace=%v",
						i, step, desc, err, seed, trace)
				}
				if m.status != cmd.to && rule == nil {
					t.Fatalf("iteration %d step %d: non-no-op transition %s returned nil rule",
						i, step, desc)
				}
				if rule != nil {
					if rule.From != expectedRule.From || rule.To != expectedRule.To ||
						rule.Authoritative != expectedRule.Authoritative || rule.UpdatesSynced != expectedRule.UpdatesSynced {
						t.Fatalf("iteration %d step %d: rule mismatch for %s: got %+v want %+v",
							i, step, desc, rule, expectedRule)
					}
				}
				m.apply(rule, cmd.to)
			} else {
				if err == nil {
					t.Fatalf("iteration %d step %d: expected error for %s, got success\nseed=%d trace=%v",
						i, step, desc, seed, trace)
				}
			}

			if !isKnownStatus(m.status) {
				t.Fatalf("iteration %d step %d: model entered unknown status %q\nseed=%d trace=%v",
					i, step, m.status, seed, trace)
			}
		}
	}
}

// TestStateMachine_KnownSeeds runs the same harness over a small set of fixed
// seeds. This keeps CI deterministic while still covering sequences that are
// too long to enumerate by hand.
func TestStateMachine_KnownSeeds(t *testing.T) {
	for _, seed := range []uint64{0, 1, 42, 1337, 9999} {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			runStateMachineSequence(t, seed, 30)
		})
	}
}

// runStateMachineSequence is a deterministic, single-sequence version of the
// randomized test used by TestStateMachine_KnownSeeds.
func runStateMachineSequence(t *testing.T, seed uint64, steps int) {
	rng := rand.New(rand.NewPCG(seed, 0))
	t.Logf("state-machine seed: %d", seed)

	statuses := AllStatuses()
	m := &smModel{
		status:       statuses[rng.IntN(len(statuses))],
		syncedStatus: "",
	}
	m.syncedStatus = m.status

	trace := make([]string, 0, steps)
	for step := 0; step < steps; step++ {
		cmd := randomCommand(rng, m.status)
		desc := fmt.Sprintf("%s->%s(auth=%v)", m.status, cmd.to, cmd.authoritative)
		trace = append(trace, desc)

		expectedRule, expectOK := expectedOutcome(m.status, cmd.to, cmd.authoritative)
		rule, err := ValidateTransition(m.status, cmd.to, cmd.authoritative)

		if expectOK {
			if err != nil {
				t.Fatalf("step %d: expected success for %s, got error: %v\ntrace=%v", step, desc, err, trace)
			}
			if m.status != cmd.to && rule == nil {
				t.Fatalf("step %d: non-no-op transition %s returned nil rule", step, desc)
			}
			if rule != nil && (rule.From != expectedRule.From || rule.To != expectedRule.To ||
				rule.Authoritative != expectedRule.Authoritative || rule.UpdatesSynced != expectedRule.UpdatesSynced) {
				t.Fatalf("step %d: rule mismatch for %s: got %+v want %+v", step, desc, rule, expectedRule)
			}
			m.apply(rule, cmd.to)
		} else {
			if err == nil {
				t.Fatalf("step %d: expected error for %s, got success\ntrace=%v", step, desc, trace)
			}
		}
	}
}

// smModel is the reference state for one job attempt.
type smModel struct {
	status       string
	syncedStatus string
}

func (m *smModel) apply(rule *TransitionRule, to string) {
	m.status = to
	if rule != nil && rule.UpdatesSynced {
		m.syncedStatus = to
	}
}

// smCommand is one transition attempt.
type smCommand struct {
	to            string
	authoritative bool
}

// randomCommand returns either a valid transition from the current status or a
// deliberately invalid one, so the test covers both acceptance and rejection
// paths.
func randomCommand(rng *rand.Rand, from string) smCommand {
	statuses := AllStatuses()

	// 60% of the time, try a transition that is allowed by the spec table.
	if rng.Float64() < 0.6 {
		allowed := transitionsFrom(from)
		if len(allowed) > 0 {
			rule := allowed[rng.IntN(len(allowed))]
			return smCommand{to: rule.To, authoritative: rule.Authoritative}
		}
	}

	// Otherwise pick a random target status and authoritative flag. This will
	// frequently be illegal, which is the point: the implementation must reject
	// it.
	return smCommand{
		to:            statuses[rng.IntN(len(statuses))],
		authoritative: rng.IntN(4) == 0, // 25% authoritative
	}
}

// expectedOutcome uses the package's transition table as the oracle. The
// implementation under test is the lookup logic in ValidateTransition and its
// handling of the authoritative flag.
func expectedOutcome(from, to string, authoritative bool) (*TransitionRule, bool) {
	if from == to {
		return nil, true
	}
	rule, ok := transitionIndex[from+"\x00"+to]
	if !ok {
		return nil, false
	}
	if rule.Authoritative && !authoritative {
		return rule, false
	}
	return rule, true
}

func transitionsFrom(from string) []*TransitionRule {
	var out []*TransitionRule
	for i := range transitions {
		if transitions[i].From == from {
			out = append(out, &transitions[i])
		}
	}
	return out
}

func isKnownStatus(s string) bool {
	for _, known := range AllStatuses() {
		if known == s {
			return true
		}
	}
	return false
}

func testSeed() uint64 {
	if s := os.Getenv("WEFT_TEST_SEED"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return n
		}
	}
	return rand.Uint64()
}
