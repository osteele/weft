package gpucatalog

import "testing"

// MatchesPartName answers the NAME axis only. A part name never carries a
// capacity — "A100 SXM4" names both the 40GB and 80GB part — so a memory token
// in the constraint must not affect it. Two matchers that disagreed on exactly
// this point are what made one constraint string behave three ways depending
// on which path evaluated it.
func TestMatchesPartName_IgnoresMemoryToken(t *testing.T) {
	if !MatchesPartName("a100sxm480gb", "a100sxm4") {
		t.Error("a memory token must not prevent a name-axis match")
	}
	if !MatchesPartName("a100sxm4", "a100sxm4") {
		t.Error("bare class must match itself")
	}
}

func TestMatchesPartName_VariantsAndAliases(t *testing.T) {
	cases := []struct {
		constraint, candidate string
		want                  bool
	}{
		// A bare base class accepts any variant: the user named no preference.
		{"h100", "h100nvl", true},
		{"h100", "h100sxm", true},
		{"h100", "h100pcie", true},
		// A named variant does not accept a different one.
		{"h100nvl", "h100sxm", false},
		{"h100pcie", "h100nvl", false},
		// Aliases denote the same part.
		{"h100hbm3", "h100sxm", true},
		{"a100sxm", "a100sxm4", true},
		// Bare RTX model numbers resolve to their rtx-prefixed spelling.
		{"3090", "rtx3090", true},
		// Unrelated parts do not match.
		{"a100sxm4", "h100sxm", false},
		{"", "a100sxm4", false},
		{"a100sxm4", "", false},
	}
	for _, tc := range cases {
		if got := MatchesPartName(tc.constraint, tc.candidate); got != tc.want {
			t.Errorf("MatchesPartName(%q, %q) = %v, want %v", tc.constraint, tc.candidate, got, tc.want)
		}
	}
}
