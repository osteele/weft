package status

import (
	"slices"
	"testing"
)

func TestIsTerminal(t *testing.T) {
	terminal := []string{Completed, Dead, Failed, Killed, Canceled, Draft}
	nonTerminal := []string{Starting, Running, Queued, Paused, PendingPlacement}

	for _, s := range terminal {
		if !IsTerminal(s) {
			t.Errorf("expected %s to be terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if IsTerminal(s) {
			t.Errorf("expected %s to be non-terminal", s)
		}
	}
}

func TestAllStatusesCovered(t *testing.T) {
	all := AllStatuses()
	// Every status should appear as From in at least one rule OR be Completed
	// (which is a sink state with no non-authoritative outbound transitions).
	for _, s := range all {
		if s == Completed {
			continue // sink state
		}
		allowed := AllowedFrom(s, false)
		if len(allowed) == 0 {
			t.Errorf("status %s has no non-authoritative outbound transitions", s)
		}
	}
}

func TestValidateTransition_NoOp(t *testing.T) {
	rule, err := ValidateTransition(Running, Running, false)
	if err != nil {
		t.Fatalf("no-op transition should succeed: %v", err)
	}
	if rule != nil {
		t.Fatal("no-op transition should return nil rule")
	}
}

func TestValidateTransition_Valid(t *testing.T) {
	cases := []struct {
		from, to      string
		authoritative bool
	}{
		{Queued, Running, false},
		{Starting, Running, false},
		{Running, Completed, false},
		{Running, Failed, false},
		{Running, Paused, false},
		{Paused, Running, false},
		{Failed, Queued, false},
		{Dead, Running, false},
		{Draft, Queued, false},
		// Authoritative
		{Failed, Completed, true},
		{Dead, Completed, true},
	}
	for _, tc := range cases {
		rule, err := ValidateTransition(tc.from, tc.to, tc.authoritative)
		if err != nil {
			t.Errorf("ValidateTransition(%s, %s, %v) = error %v; want success", tc.from, tc.to, tc.authoritative, err)
		}
		if rule == nil {
			t.Errorf("ValidateTransition(%s, %s, %v) = nil rule; want non-nil", tc.from, tc.to, tc.authoritative)
		}
	}
}

func TestValidateTransition_Invalid(t *testing.T) {
	cases := []struct {
		from, to      string
		authoritative bool
	}{
		// Completed is a sink (non-authoritative)
		{Completed, Running, false},
		{Completed, Queued, false},
		// Cannot go backward from completed
		{Completed, Starting, false},
		// Authoritative required but not provided
		{Failed, Completed, false},
		{Dead, Completed, false},
		// Nonsensical transitions
		{Running, Starting, false},
		{Paused, Starting, false},
	}
	for _, tc := range cases {
		_, err := ValidateTransition(tc.from, tc.to, tc.authoritative)
		if err == nil {
			t.Errorf("ValidateTransition(%s, %s, %v) = nil; want error", tc.from, tc.to, tc.authoritative)
		}
	}
}

func TestValidateTransition_AuthoritativeRequired(t *testing.T) {
	// Failed->Completed exists but requires authoritative
	_, err := ValidateTransition(Failed, Completed, false)
	if err == nil {
		t.Fatal("expected error for non-authoritative Failed->Completed")
	}
	ite, ok := err.(*InvalidTransitionError)
	if !ok {
		t.Fatalf("expected *InvalidTransitionError, got %T", err)
	}
	if ite.Reason != "transition requires authoritative source" {
		t.Errorf("unexpected reason: %s", ite.Reason)
	}

	// Same transition with authoritative should succeed
	rule, err := ValidateTransition(Failed, Completed, true)
	if err != nil {
		t.Fatalf("authoritative Failed->Completed should succeed: %v", err)
	}
	if !rule.Authoritative {
		t.Error("expected rule to be authoritative")
	}
	if !rule.UpdatesSynced {
		t.Error("expected authoritative completion to update synced status")
	}
}

func TestAllowedFrom(t *testing.T) {
	// Queued should reach several states (non-authoritative)
	allowed := AllowedFrom(Queued, false)
	for _, expected := range []string{Running, Canceled, Killed, Dead, Paused, Draft, Starting, Failed} {
		if !slices.Contains(allowed, expected) {
			t.Errorf("AllowedFrom(%s, false) missing %s; got %v", Queued, expected, allowed)
		}
	}

	// Completed has no non-authoritative outbound transitions
	allowed = AllowedFrom(Completed, false)
	if len(allowed) != 0 {
		t.Errorf("AllowedFrom(%s, false) = %v; want empty", Completed, allowed)
	}

	// Failed should include Completed when authoritative
	allowed = AllowedFrom(Failed, true)
	if !slices.Contains(allowed, Completed) {
		t.Errorf("AllowedFrom(%s, true) missing %s; got %v", Failed, Completed, allowed)
	}
}

func TestTransitionRuleProperties(t *testing.T) {
	// All authoritative rules should update synced status
	for _, r := range transitions {
		if r.Authoritative && !r.UpdatesSynced {
			t.Errorf("authoritative rule %s->%s should update synced status", r.From, r.To)
		}
	}

	// No duplicate rules
	seen := make(map[string]bool)
	for _, r := range transitions {
		key := r.From + "->" + r.To
		if seen[key] {
			t.Errorf("duplicate transition rule: %s", key)
		}
		seen[key] = true
	}

	// All From and To values should be valid statuses
	allSet := make(map[string]bool)
	for _, s := range AllStatuses() {
		allSet[s] = true
	}
	for _, r := range transitions {
		if !allSet[r.From] {
			t.Errorf("transition rule has unknown From status: %s", r.From)
		}
		if !allSet[r.To] {
			t.Errorf("transition rule has unknown To status: %s", r.To)
		}
	}
}

func TestInvalidTransitionError(t *testing.T) {
	err := &InvalidTransitionError{From: "running", To: "starting", Reason: "not allowed"}
	expected := "invalid status transition running -> starting: not allowed"
	if err.Error() != expected {
		t.Errorf("got %q; want %q", err.Error(), expected)
	}
}
