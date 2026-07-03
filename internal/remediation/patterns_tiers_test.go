package remediation

import "testing"

func failureRuleIndex(t *testing.T, id string) int {
	t.Helper()
	for i, r := range failurePatternRules {
		if r.patternID == id {
			return i
		}
	}
	t.Fatalf("rule %q not found in failurePatternRules", id)
	return -1
}

// The assembled failurePatternRules slice must be exactly the tier slices
// concatenated in the declared tier order. First-match-wins depends on this.
func TestFailurePatternTiers_OrderingPreserved(t *testing.T) {
	var want []string
	for _, tier := range failurePatternTiers {
		if len(tier.rules) == 0 {
			t.Errorf("tier %q is empty", tier.name)
		}
		for _, r := range tier.rules {
			want = append(want, r.patternID)
		}
	}
	if len(failurePatternRules) != len(want) {
		t.Fatalf("assembled rules = %d entries, tiers hold %d", len(failurePatternRules), len(want))
	}
	for i, r := range failurePatternRules {
		if r.patternID != want[i] {
			t.Errorf("failurePatternRules[%d] = %q, want %q (tier order violated)", i, r.patternID, want[i])
		}
	}
}

// Pattern IDs must be unique within the assembled table; a duplicate would
// silently shadow the later rule.
func TestFailurePatternTiers_UniqueIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range failurePatternRules {
		if seen[r.patternID] {
			t.Errorf("duplicate pattern ID %q in failurePatternRules", r.patternID)
		}
		seen[r.patternID] = true
	}
}

// Every rule in a non-generic tier must precede every rule in the generic
// tier: the generic catch-alls (module_not_found, assert_failure,
// subprocess_failure) match wide classes of failures that the earlier tiers
// diagnose more specifically.
func TestFailurePatternTiers_SpecificPrecedesGeneric(t *testing.T) {
	genericIDs := map[string]bool{}
	for _, r := range genericRules {
		genericIDs[r.patternID] = true
	}
	maxNonGeneric, minGeneric := -1, len(failurePatternRules)
	for i, r := range failurePatternRules {
		if genericIDs[r.patternID] {
			if i < minGeneric {
				minGeneric = i
			}
		} else if i > maxNonGeneric {
			maxNonGeneric = i
		}
	}
	if maxNonGeneric > minGeneric {
		t.Errorf("a non-generic rule (index %d) follows a generic rule (index %d)", maxNonGeneric, minGeneric)
	}
}

// wb27: a first-party ImportError (e.g. `experiments` missing from sys.path)
// must classify as python_first_party_import_path, which requires that rule
// to precede both the framework-setup rules and the generic
// module_not_found rule. Behavior-level coverage lives in
// TestDiagnoseFromLog_FirstPartyImportPathBeatsVLLMSetup; this pins the
// ordering itself.
func TestFailurePatternTiers_FirstPartyImportPrecedence(t *testing.T) {
	firstParty := failureRuleIndex(t, "python_first_party_import_path")
	for _, id := range []string{"sglang_setup", "vllm_setup", "module_not_found"} {
		if idx := failureRuleIndex(t, id); idx <= firstParty {
			t.Errorf("%s (index %d) must follow python_first_party_import_path (index %d)", id, idx, firstParty)
		}
	}
}
