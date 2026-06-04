// Tests derived from specs/job-lifecycle.allium transition graph.
// Every declared edge must be accepted; every undeclared edge must be rejected.
// Terminal states must have no non-authoritative outbound transitions.
package status

import (
	"fmt"
	"testing"
)

// allDeclaredTransitions is the complete set of edges from the spec's
// "transitions status" block, transcribed verbatim.
var allDeclaredTransitions = []struct {
	from, to      string
	authoritative bool
}{
	// Normal forward flow
	{Draft, Queued, false},
	{PendingPlacement, Queued, false},
	{PendingPlacement, Canceled, false},
	{Queued, Starting, false},
	{Queued, Running, false},
	{Queued, Paused, false},
	{Queued, Canceled, false},
	{Queued, Killed, false},
	{Queued, Dead, false},
	{Queued, Failed, false},
	{Queued, Draft, false},
	{Starting, Running, false},
	{Starting, Completed, false},
	{Starting, Dead, false},
	{Starting, Failed, false},
	{Starting, Queued, false},
	{Starting, Draft, false},
	{Running, Completed, false},
	{Running, Failed, false},
	{Running, Dead, false},
	{Running, Killed, false},
	{Running, Canceled, false},
	{Running, Paused, false},
	{Running, Queued, false},
	{Running, Draft, false},
	{Paused, Running, false},
	{Paused, Killed, false},
	{Paused, Failed, false},
	{Paused, Queued, false},

	// Restart/requeue paths (from terminal states)
	{Completed, Queued, false},
	{Failed, Running, false},
	{Failed, Paused, false},
	{Failed, Queued, false},
	{Dead, Running, false},
	{Dead, Paused, false},
	{Dead, Queued, false},
	{Killed, Paused, false},
	{Killed, Queued, false},
	{Canceled, Paused, false},
	{Canceled, Queued, false},
	{Canceled, Failed, false},

	// Authoritative overrides (R2 completion)
	{Queued, Completed, true},
	{Failed, Completed, true},
	{Dead, Completed, true},
	{Killed, Completed, true},
	{Canceled, Completed, true},
}

func TestSpec_EveryDeclaredTransitionIsAccepted(t *testing.T) {
	for _, tc := range allDeclaredTransitions {
		name := fmt.Sprintf("%s->%s(auth=%v)", tc.from, tc.to, tc.authoritative)
		t.Run(name, func(t *testing.T) {
			rule, err := ValidateTransition(tc.from, tc.to, tc.authoritative)
			if err != nil {
				t.Fatalf("spec-declared transition rejected: %v", err)
			}
			if rule == nil {
				t.Fatal("expected non-nil rule for declared transition")
			}
		})
	}
}

func TestSpec_UndeclaredTransitionsAreRejected(t *testing.T) {
	// Build set of declared edges.
	declared := make(map[string]bool)
	for _, tc := range allDeclaredTransitions {
		declared[tc.from+"\x00"+tc.to] = true
	}

	allStatuses := AllStatuses()
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			if from == to {
				continue // no-ops always allowed
			}
			key := from + "\x00" + to
			if declared[key] {
				continue // skip declared edges
			}
			name := fmt.Sprintf("%s->%s", from, to)
			t.Run(name, func(t *testing.T) {
				// Non-authoritative must fail
				_, err := ValidateTransition(from, to, false)
				if err == nil {
					t.Errorf("undeclared transition %s -> %s was accepted (non-authoritative)", from, to)
				}
			})
		}
	}
}

func TestSpec_TerminalStatesHaveNoNonAuthoritativeOutbound(t *testing.T) {
	terminalStates := []string{Completed, Dead, Failed, Killed, Canceled, Draft}
	for _, s := range terminalStates {
		t.Run(s, func(t *testing.T) {
			allowed := AllowedFrom(s, false)
			// Terminal states may have restart paths.
			if len(allowed) == 0 {
				t.Errorf("terminal state %s should have a restart/requeue path", s)
			}
		})
	}
}

func TestSpec_NonTerminalStatesHaveAtLeastOneExit(t *testing.T) {
	nonTerminal := []string{Starting, Running, Queued, Paused, PendingPlacement}
	for _, s := range nonTerminal {
		t.Run(s, func(t *testing.T) {
			allowed := AllowedFrom(s, false)
			if len(allowed) == 0 {
				t.Errorf("non-terminal state %s has no outbound transitions", s)
			}
		})
	}
}

func TestSpec_AuthoritativeTransitionsUpdateSyncedStatus(t *testing.T) {
	for _, tc := range allDeclaredTransitions {
		if !tc.authoritative {
			continue
		}
		name := fmt.Sprintf("%s->%s", tc.from, tc.to)
		t.Run(name, func(t *testing.T) {
			rule, err := ValidateTransition(tc.from, tc.to, true)
			if err != nil {
				t.Fatalf("authoritative transition failed: %v", err)
			}
			if !rule.UpdatesSynced {
				t.Errorf("authoritative transition %s->%s should update synced status", tc.from, tc.to)
			}
		})
	}
}

func TestSpec_AuthoritativeTransitionsRequireFlag(t *testing.T) {
	for _, tc := range allDeclaredTransitions {
		if !tc.authoritative {
			continue
		}
		name := fmt.Sprintf("%s->%s", tc.from, tc.to)
		t.Run(name, func(t *testing.T) {
			_, err := ValidateTransition(tc.from, tc.to, false)
			if err == nil {
				t.Errorf("authoritative transition %s->%s should fail without authoritative flag", tc.from, tc.to)
			}
		})
	}
}

func TestSpec_CompletedOnlyRequeues(t *testing.T) {
	allowed := AllowedFrom(Completed, true)
	if len(allowed) != 1 || allowed[0] != Queued {
		t.Errorf("Completed should only requeue; got %v", allowed)
	}
}
