package gpucatalog

import "testing"

// A trailing memory token in a GPU class is decorative — provider gpu_name
// values carry no memory component, so matching strips it. Detecting it is how
// weft warns that `--gpu a100-sxm4-80gb` did not actually request 80GB, and
// the base half is what names the binding form (`a100-sxm4>=80GB`).
func TestSplitTrailingMemorySuffix(t *testing.T) {
	tests := []struct {
		class    string
		wantBase string
		wantGB   int
	}{
		{"a100-sxm4-80gb", "a100-sxm4", 80},
		{"a100-pcie-80gb", "a100-pcie", 80},
		{"A100-SXM4-80GB", "a100-sxm4", 80},
		{"rtx-a6000-48gb", "rtx-a6000", 48},
		{"  a100-pcie-80gb  ", "a100-pcie", 80},
		// No trailing memory token.
		{"a100", "", 0},
		{"h100-nvl", "", 0},
		{"rtx-a6000", "", 0},
		{"", "", 0},
		// The separator is what makes the token unambiguous: without one,
		// "a100sxm480gb" could be sxm4 + 80GB or sxm + 480GB, so we decline
		// rather than guess.
		{"a100sxm480gb", "", 0},
		// A bare size is not a GPU class, with or without a leading separator:
		// there is no variant to carry into the binding form.
		{"80gb", "", 0},
		{"-80gb", "", 0},
		// Non-numeric before "gb" is not a size.
		{"some-thing-xgb", "", 0},
	}
	for _, tt := range tests {
		base, gb := SplitTrailingMemorySuffix(tt.class)
		if base != tt.wantBase || gb != tt.wantGB {
			t.Errorf("SplitTrailingMemorySuffix(%q) = (%q, %d), want (%q, %d)",
				tt.class, base, gb, tt.wantBase, tt.wantGB)
		}
	}
}
