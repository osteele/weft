package cmd

import (
	"strings"
	"testing"
)

// A memory size inside a GPU class is an exact SKU selector, so a size the
// part does not ship in can never match. Once placement enforces that
// exactly, an unmatchable spec is indistinguishable at the blocker string
// from a market that happens to be empty — so it has to fail at submit,
// where the user can still fix the typo.
func TestValidateGPUSKUMemory_RejectsUnknownSize(t *testing.T) {
	err := validateGPUSKUMemory("a100-sxm4-64gb")
	if err == nil {
		t.Fatal("want an error: A100 SXM4 does not ship in 64GB")
	}
	for _, want := range []string{"64GB", "40GB", "80GB", `"a100-sxm4>=64GB"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err.Error(), want)
		}
	}
}

// Known sizes are listed by capacity. A lexical sort of the formatted labels
// renders the RTX 4060's {8, 16} as "16GB, 8GB".
func TestValidateGPUSKUMemory_ListsSizesByCapacity(t *testing.T) {
	err := validateGPUSKUMemory("rtx4060-24gb")
	if err == nil {
		t.Fatal("want an error: RTX 4060 does not ship in 24GB")
	}
	if !strings.Contains(err.Error(), "known sizes: 8GB, 16GB") {
		t.Errorf("error = %q, want sizes listed as \"8GB, 16GB\"", err.Error())
	}
}

func TestValidateGPUSKUMemory_Accepts(t *testing.T) {
	cases := []struct {
		name  string
		class string
	}{
		{"real SKU", "a100-sxm4-80gb"},
		{"other real SKU", "a100-sxm4-40gb"},
		{"no memory token", "a100-sxm4"},
		{"family", "nvidia"},
		{"generation", "ampere+"},
		{"empty", ""},
		// Weft cannot validate a part it has never heard of, and refusing one
		// would make a stale catalogue look like a bad request.
		{"uncatalogued class", "some-future-gpu-32gb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateGPUSKUMemory(tc.class); err != nil {
				t.Errorf("validateGPUSKUMemory(%q) = %v, want nil", tc.class, err)
			}
		})
	}
}
