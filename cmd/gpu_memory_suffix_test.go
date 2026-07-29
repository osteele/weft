package cmd

import (
	"strings"
	"testing"
)

// Regression for a silent hardware substitution: EXP-440 requested
// --gpu a100-pcie-80gb and was served an A100 PCIE 40GB, with nothing in the
// job's own output contradicting the request. The memory suffix binds nothing;
// only a `>=NGB` predicate does.
func TestGPUMemorySuffixWarning(t *testing.T) {
	got := gpuMemorySuffixWarning("a100-pcie-80gb", 40)
	if got == "" {
		t.Fatal("want a warning when delivered memory is below the requested suffix")
	}
	for _, want := range []string{"a100-pcie-80gb", "40GB", "not binding", `"a100-pcie>=80GB"`} {
		if !strings.Contains(got, want) {
			t.Errorf("warning = %q, want it to contain %q", got, want)
		}
	}
}

func TestGPUMemorySuffixWarning_Silent(t *testing.T) {
	cases := []struct {
		name        string
		class       string
		deliveredGB int
	}{
		// Family request with no memory token: H100 NVL is a legitimate H100.
		{"no suffix requested", "h100", 94},
		// The suffix was satisfied.
		{"suffix satisfied", "a100-sxm4-80gb", 80},
		{"delivered exceeds request", "a100-sxm4-40gb", 80},
		// Nothing known about the delivered hardware.
		{"delivered unknown", "a100-sxm4-80gb", 0},
		{"no class", "", 40},
		// Drivers report usable memory, a few percent below the nominal SKU
		// capacity. A correctly-served A100 40GB reports 39GB and an RTX A5000
		// 24GB reports 22GB; neither is a substitution. Reported by a user
		// after the first version warned on every such card, which would have
		// trained people to ignore the warning.
		{"usable below nominal, A100 40GB", "a100-sxm4-40gb", 39},
		{"usable below nominal, A5000 24GB", "rtx-a5000-24gb", 22},
		{"usable below nominal, A100 80GB", "a100-sxm4-80gb", 79},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gpuMemorySuffixWarning(tc.class, tc.deliveredGB); got != "" {
				t.Errorf("want no warning, got %q", got)
			}
		})
	}
}

// The SKU rounding that silences the usable-vs-nominal false positive must not
// silence a real substitution reported the same way: wj5509 asked for 80GB and
// was served an A100 SXM4 whose driver reported 39GB. 39 rounds to the 40GB
// SKU, which is still a shortfall against 80.
func TestGPUMemorySuffixWarning_UsableMemoryStillCatchesSubstitution(t *testing.T) {
	got := gpuMemorySuffixWarning("a100-sxm4-80gb", 39)
	if got == "" {
		t.Fatal("want a warning: 39GB rounds to the 40GB SKU, short of the requested 80GB")
	}
	if !strings.Contains(got, `"a100-sxm4>=80GB"`) {
		t.Errorf("warning = %q, want the binding remediation form", got)
	}
}
