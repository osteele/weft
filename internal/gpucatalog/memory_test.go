package gpucatalog

import (
	"slices"
	"testing"
)

func TestKnownHardwareMemoryGB(t *testing.T) {
	cases := []struct {
		class string
		mem   int
		want  bool
	}{
		{"a100", 80, true},
		{"A100 PCIE", 80, true},
		{"A100 SXM4", 80, true},
		{"a100", 40, true},
		{"a100", 50, false},
		{"a100", 82, false},
		{"h100", 80, true},
		{"H100 PCIe", 80, true},
		// wj3135: the user-facing class "t4" must reach the Tesla T4's 16GB
		// ceiling, which was once keyed only under the gpu_name spelling.
		{"t4", 16, true},
		{"", 80, false},
		{"nvidia", 80, false},
	}
	for _, tc := range cases {
		if got := KnownHardwareMemoryGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("KnownHardwareMemoryGB(%q, %d) = %v, want %v", tc.class, tc.mem, got, tc.want)
		}
	}
}

func TestKnownLargeHardwareMemoryGB(t *testing.T) {
	cases := []struct {
		mem  int
		want bool
	}{
		{8, false},
		{10, false},
		{16, false},
		{24, true},
		{32, true},
		{40, true},
		{48, true},
		{50, false},
		{80, true},
		{141, true},
	}
	for _, tc := range cases {
		if got := KnownLargeHardwareMemoryGB(tc.mem); got != tc.want {
			t.Errorf("KnownLargeHardwareMemoryGB(%d) = %v, want %v", tc.mem, got, tc.want)
		}
	}
}

// TestHardwareMemoryTableHasCorePinnedModels guards against accidental
// edits that drop capacities the rest of the codebase relies on. These are
// now derived from Entries, so a dropped Entry.MemoryGB fails here too.
func TestHardwareMemoryTableHasCorePinnedModels(t *testing.T) {
	required := []string{"a100", "h100", "h200", "rtx4090", "rtx5090"}
	for _, r := range required {
		if _, ok := MemorySizesGB(r); !ok {
			t.Errorf("no catalogued capacity for required class %q", r)
		}
	}
}

// TestMemorySizesGBReturnsCopy guards the table against callers that sort or
// otherwise rewrite the slice they get back — cmd.validateGPUSKUMemory sorts
// it to render sizes in capacity order.
func TestMemorySizesGBReturnsCopy(t *testing.T) {
	sizes, ok := MemorySizesGB("a100")
	if !ok || len(sizes) < 2 {
		t.Fatalf("MemorySizesGB(\"a100\") = (%v, %v), want at least two sizes", sizes, ok)
	}
	slices.Reverse(sizes)
	again, _ := MemorySizesGB("a100")
	if !slices.IsSorted(again) {
		t.Errorf("mutating the returned slice corrupted the table: got %v", again)
	}
}

// TestMaxHardwareMemGB covers the ceiling lookup used to clamp the blanket
// default reservation down to a named card's real VRAM.
func TestMaxHardwareMemGB(t *testing.T) {
	cases := []struct {
		class  string
		wantGB int
		wantOK bool
	}{
		{"t4", 16, true},   // single-size small card — the wj3135 case
		{"T4", 16, true},   // case-insensitive
		{"a100", 80, true}, // multi-size: max of {40,80}
		{"rtx2080ti", 11, true},
		{"a6000", 48, true},   // bare workstation class resolves via rtx-prefix retry
		{"v100", 32, true},    // max of {16,32}
		{"nvidia", 0, false},  // family — no single ceiling
		{"ampere+", 0, false}, // generation — no single ceiling
		{"", 0, false},
		{"0,1", 0, false}, // device index, not a class
	}
	for _, tc := range cases {
		gotGB, gotOK := MaxHardwareMemGB(tc.class)
		if gotGB != tc.wantGB || gotOK != tc.wantOK {
			t.Errorf("MaxHardwareMemGB(%q) = (%d, %v), want (%d, %v)", tc.class, gotGB, gotOK, tc.wantGB, tc.wantOK)
		}
	}
}
