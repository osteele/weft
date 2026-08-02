package campaign

import (
	"slices"
	"testing"
)

// Every exported placement.Constraints field must carry an enforce tag — an
// axis name or "-". A field without one is a constraint whose enforcement
// nobody decided, which is the state every bug in the pin/interconnect/
// host-axis family started from.
func TestEveryConstraintFieldCarriesEnforceTag(t *testing.T) {
	for _, name := range untaggedConstraintFields {
		t.Errorf("placement.Constraints.%s has no enforce tag: name its constraint axis, "+
			"or tag it enforce:\"-\" if it is not a hard constraint", name)
	}
}

// Every axis must be accounted for in every placement system. The third case
// — an axis some system neither checks nor declares out of scope — is the
// invisible one: the constraint is accepted at submit and silently unmet
// whenever that system happens to serve the job.
func TestConstraintSystemCoverage(t *testing.T) {
	axes := ConstraintAxes()
	for _, system := range AllPlacementSystems() {
		byAxis, ok := constraintSystemEnforcement[system]
		if !ok {
			t.Errorf("placement system %q has no enforcement rows", system)
			continue
		}
		for _, axis := range axes {
			if _, recorded := byAxis[axis]; !recorded {
				t.Errorf("system %q does not record who checks constraint axis %q.\n"+
					"Add it with the predicate that checks it, or why the system has "+
					"nothing to check. Machine pins, interconnect, host RAM, CPU cores, "+
					"and the driver floor each shipped checked in one system and "+
					"silently ignored in another.", system, axis)
			}
		}
		for axis := range byAxis {
			if !slices.Contains(axes, axis) {
				t.Errorf("system %q records unknown axis %q; the vocabulary comes from "+
					"placement.Constraints enforce tags", system, axis)
			}
		}
	}
}

// An axis no system checks is accepted from users and enforced by nobody.
func TestNoConstraintAxisIsUncheckedEverywhere(t *testing.T) {
	for _, axis := range ConstraintAxes() {
		anywhere := false
		for _, byAxis := range constraintSystemEnforcement {
			if note, ok := byAxis[axis]; ok && note.by == checkedInSystem {
				anywhere = true
				break
			}
		}
		if !anywhere {
			t.Errorf("constraint axis %q is checked by no placement system", axis)
		}
	}
}

// unenforcedSystemGap entries are known holes and must stay rare and pinned:
// closing one should update this test, and adding one should be a deliberate
// act rather than a quiet regression. No gaps are pinned — every gap the
// 2026-08 audit found is closed — so any gap entry fails. When pinning a
// deliberate gap, reintroduce a want-list here (see TestKnownEnforcementGaps
// for the shape).
func TestKnownConstraintSystemGaps(t *testing.T) {
	for system, byAxis := range constraintSystemEnforcement {
		for axis, note := range byAxis {
			if note.by == unenforcedSystemGap {
				t.Errorf("new unenforced gap: system %q does not check %q (%s).\n"+
					"Either enforce it or pin it in this test with a reason.",
					system, axis, note.detail)
			}
		}
	}
}
