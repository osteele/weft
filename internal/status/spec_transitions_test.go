// Tests derived from specs/job-lifecycle.allium transition graph.
// The spec's "transitions status" block is parsed (not re-transcribed) so
// this file and the code table in status.go cannot drift apart silently:
// TestSpec_CodeTableMatchesSpec asserts set-equality in both directions,
// including the authoritative annotation on override edges.
//
// Every declared edge must be accepted; every undeclared edge must be
// rejected. Terminal states must have no non-authoritative outbound
// transitions.
package status

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// specEdge is one edge parsed from the spec's transitions block.
type specEdge struct {
	from, to      string
	authoritative bool
}

// specTransitionsPath locates specs/job-lifecycle.allium relative to this
// package. The spec ships in-tree; a missing file is a hard failure, not a
// skip, because the drift check is the point of these tests.
func specTransitionsPath() (string, error) {
	for _, candidate := range []string{
		filepath.Join("..", "..", "specs", "job-lifecycle.allium"),
		filepath.Join("specs", "job-lifecycle.allium"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("specs/job-lifecycle.allium not found relative to internal/status")
}

// parseSpecTransitions extracts the Job entity's "transitions status" block.
// An edge carries authoritative=true when its trailing comment says
// "authoritative"; the "terminal:" line is returned separately.
func parseSpecTransitions(t *testing.T) (edges []specEdge, terminal []string) {
	t.Helper()
	path, err := specTransitionsPath()
	if err != nil {
		t.Fatalf("locate spec: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	lines := strings.Split(string(data), "\n")

	inBlock := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if trimmed == "transitions status {" {
				inBlock = true
			}
			continue
		}
		if trimmed == "}" {
			return edges, terminal
		}
		body, _, _ := strings.Cut(trimmed, "--")
		body = strings.TrimSpace(body)
		if body == "" {
			continue
		}
		if strings.HasPrefix(body, "terminal:") {
			for _, name := range strings.Split(strings.TrimPrefix(body, "terminal:"), ",") {
				if name := strings.TrimSpace(name); name != "" {
					terminal = append(terminal, name)
				}
			}
			continue
		}
		from, to, ok := strings.Cut(body, "->")
		if !ok {
			t.Fatalf("%s:%d: not an edge or terminal line: %q", path, i+1, trimmed)
		}
		edges = append(edges, specEdge{
			from:          strings.TrimSpace(from),
			to:            strings.TrimSpace(to),
			authoritative: strings.Contains(trimmed, "authoritative"),
		})
	}
	t.Fatalf("%s: transitions status block not terminated", path)
	return nil, nil
}

// codeEdges exposes the master table for comparison.
func codeEdges() []specEdge {
	var out []specEdge
	for i := range transitions {
		out = append(out, specEdge{
			from:          transitions[i].From,
			to:            transitions[i].To,
			authoritative: transitions[i].Authoritative,
		})
	}
	return out
}

type edgeKey struct {
	from, to string
}

func edgeSet(edges []specEdge) map[edgeKey]specEdge {
	set := make(map[edgeKey]specEdge, len(edges))
	for _, e := range edges {
		set[edgeKey{e.from, e.to}] = e
	}
	return set
}

// TestSpec_CodeTableMatchesSpec asserts set-equality between the spec block
// and the transitions table in status.go: same edges, same authoritative
// flags, neither side holding an edge the other lacks.
func TestSpec_CodeTableMatchesSpec(t *testing.T) {
	specEdges, _ := parseSpecTransitions(t)
	if len(specEdges) == 0 {
		t.Fatal("parsed no edges from spec; parser or spec is broken")
	}
	specSet, codeSet := edgeSet(specEdges), edgeSet(codeEdges())

	for key, se := range specSet {
		ce, ok := codeSet[key]
		if !ok {
			t.Errorf("spec declares %s -> %s but the code table does not", key.from, key.to)
			continue
		}
		if se.authoritative != ce.authoritative {
			t.Errorf("%s -> %s: spec authoritative=%v, code authoritative=%v",
				key.from, key.to, se.authoritative, ce.authoritative)
		}
	}
	for key := range codeSet {
		if _, ok := specSet[key]; !ok {
			t.Errorf("code table allows %s -> %s but the spec does not declare it", key.from, key.to)
		}
	}
}

// TestSpec_TerminalLineMatchesIsTerminal keeps the spec's terminal: list and
// the code's IsTerminal set identical.
func TestSpec_TerminalLineMatchesIsTerminal(t *testing.T) {
	_, specTerminal := parseSpecTransitions(t)
	if len(specTerminal) == 0 {
		t.Fatal("parsed no terminal line from spec")
	}
	want := make(map[string]bool, len(specTerminal))
	for _, s := range specTerminal {
		want[s] = true
	}
	for _, s := range AllStatuses() {
		if want[s] != IsTerminal(s) {
			t.Errorf("status %s: spec terminal=%v, code IsTerminal=%v", s, want[s], IsTerminal(s))
		}
	}
}

// TestSpec_NoCompletedToFailed pins the deliberate absence: a stale failed
// marker must never overwrite a later authoritative completed.
func TestSpec_NoCompletedToFailed(t *testing.T) {
	if _, err := ValidateTransition(Completed, Failed, true); err == nil {
		t.Error("completed -> failed must be rejected even for authoritative sources")
	}
}

func TestSpec_EveryDeclaredTransitionIsAccepted(t *testing.T) {
	specEdges, _ := parseSpecTransitions(t)
	for _, tc := range specEdges {
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
	specEdges, _ := parseSpecTransitions(t)
	declared := edgeSet(specEdges)

	allStatuses := AllStatuses()
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			if from == to {
				continue // no-ops always allowed
			}
			if _, ok := declared[edgeKey{from, to}]; ok {
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
	specEdges, _ := parseSpecTransitions(t)
	for _, tc := range specEdges {
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
	specEdges, _ := parseSpecTransitions(t)
	for _, tc := range specEdges {
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
