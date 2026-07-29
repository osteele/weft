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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gpuMemorySuffixWarning(tc.class, tc.deliveredGB); got != "" {
				t.Errorf("want no warning, got %q", got)
			}
		})
	}
}
