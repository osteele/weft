package vastai

import "testing"

// TestEffectiveMemGB_HardwareCeilingExact is the regression for wj2265 and
// peers: a user request for "A100 80GB" must produce a filter of
// `gpu_ram>=80` (matches the hardware's reported 80GB), not `>=82` (which
// excludes the exact GPU they asked for). Submission-time headroom is
// skipped when the request hits a known ceiling.
func TestEffectiveMemGB_HardwareCeilingExact(t *testing.T) {
	cases := []struct {
		class string
		mem   int
		want  int
	}{
		{"a100", 80, 80},
		{"a100", 40, 40},
		{"A100 PCIE", 80, 80},
		{"A100 SXM4", 80, 80},
		{"h100", 80, 80},
		{"H100 PCIe", 80, 80},
		{"H100 HBM3", 80, 80},
		{"h200", 141, 141},
		{"rtx4090", 24, 24},
		{"rtx5090", 32, 32},
		{"a800", 80, 80},
	}
	for _, tc := range cases {
		if got := EffectiveMemGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("EffectiveMemGB(%q, %d) = %d, want %d (exact ceiling)", tc.class, tc.mem, got, tc.want)
		}
	}
}

// TestEffectiveMemGB_RollbackPostHeadroom is the back-compat path for jobs
// persisted before the headroom moved to filter time: a stored value of
// (ceiling + defaultHeadroomGB) is recognized as the user's intent of
// `ceiling`. Without this, wj2265 (stored 82 from --gpu-class=a100
// --gpu-mem=80) would never find A100 80GB offers even after a fix to
// submission, because the DB value already had the headroom baked in.
func TestEffectiveMemGB_RollbackPostHeadroom(t *testing.T) {
	cases := []struct {
		class string
		mem   int
		want  int
	}{
		{"a100", 82, 80},
		{"a100", 42, 40},
		{"A100 PCIE", 82, 80},
		{"A100 SXM4", 82, 80},
		{"h100", 82, 80},
		{"H100 PCIe", 82, 80},
		{"H100 HBM3", 82, 80},
		{"h200", 143, 141},
		{"rtx4090", 26, 24},
		{"a40", 50, 48},
	}
	for _, tc := range cases {
		if got := EffectiveMemGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("EffectiveMemGB(%q, %d) = %d, want %d (rollback)", tc.class, tc.mem, got, tc.want)
		}
	}
}

// TestEffectiveMemGB_NonCeilingAppliesHeadroom verifies that values which
// are neither a known ceiling nor `ceiling+headroom` still get the +2GB
// headroom — these are user-supplied minima, not hardware ceilings.
func TestEffectiveMemGB_NonCeilingAppliesHeadroom(t *testing.T) {
	cases := []struct {
		class string
		mem   int
		want  int
	}{
		// A100 ceilings are 40 and 80; values not at or just above either
		// must get the +2GB headroom applied at filter time.
		{"a100", 50, 52},
		{"a100", 60, 62},
		{"a100", 100, 102},
		// rtx4090 ceiling is 24; 20GB request → 22GB filter.
		{"rtx4090", 20, 22},
	}
	for _, tc := range cases {
		if got := EffectiveMemGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("EffectiveMemGB(%q, %d) = %d, want %d (apply headroom)", tc.class, tc.mem, got, tc.want)
		}
	}
}

// TestEffectiveMemGB_UnknownClassFallsBackToHeadroom verifies the safe
// default: when the GPU class is empty or not in the table (broad family
// like "nvidia", or a model we haven't catalogued), the +2GB headroom is
// applied just as the submit-time path used to.
func TestEffectiveMemGB_UnknownClassFallsBackToHeadroom(t *testing.T) {
	cases := []struct {
		class string
		mem   int
		want  int
	}{
		{"", 24, 26},
		{"nvidia", 24, 26},
		{"unknown-model", 24, 26},
	}
	for _, tc := range cases {
		if got := EffectiveMemGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("EffectiveMemGB(%q, %d) = %d, want %d (unknown class)", tc.class, tc.mem, got, tc.want)
		}
	}
}

// TestEffectiveMemGB_NonPositiveIsPassthrough verifies that 0 and negative
// values aren't perturbed — they're a no-constraint signal upstream.
func TestEffectiveMemGB_NonPositiveIsPassthrough(t *testing.T) {
	if got := EffectiveMemGB("a100", 0); got != 0 {
		t.Errorf("EffectiveMemGB(a100, 0) = %d, want 0", got)
	}
}

// TestIntendedMemGB verifies the consumer-side resolution path: only the
// post-headroom rollback fires; no +2GB cushion is added. Used by the reuse
// compatibility check against a known instance capacity.
func TestIntendedMemGB(t *testing.T) {
	cases := []struct {
		class string
		mem   int
		want  int
	}{
		// Rollback fires for recognizable post-headroom values.
		{"a100", 82, 80},
		{"h100", 82, 80},
		{"rtx4090", 26, 24},
		// Exact ceilings pass through.
		{"a100", 80, 80},
		{"A100 PCIE", 80, 80},
		{"rtx4090", 24, 24},
		// Non-ceiling values pass through WITHOUT adding headroom — that's
		// the difference from EffectiveMemGB. A job stored as 50GB on an
		// A100 must NOT be inflated to 52GB when checking instance fit.
		{"a100", 50, 50},
		{"rtx4090", 20, 20},
		// Unknown class — pure passthrough.
		{"", 24, 24},
		{"nvidia", 24, 24},
		// Zero / negative passthrough.
		{"a100", 0, 0},
	}
	for _, tc := range cases {
		if got := IntendedMemGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("IntendedMemGB(%q, %d) = %d, want %d", tc.class, tc.mem, got, tc.want)
		}
	}
}

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
// table edits that drop the entries the rest of the codebase relies on.
func TestHardwareMemoryTableHasCorePinnedModels(t *testing.T) {
	required := []string{"a100", "h100", "h200", "rtx4090", "rtx5090"}
	classes := hardwareMemoryClassesSorted()
	have := make(map[string]bool, len(classes))
	for _, c := range classes {
		have[c] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Errorf("hardwareMemoryByClass missing required class %q", r)
		}
	}
}

// TestEffectiveMemGB_T4Reachable is the regression for wj3135: the user-facing
// class "t4" normalizes to "t4", but the table previously keyed the Tesla T4
// only under "teslat4" (the gpu_name normalization), so its 16GB ceiling was
// invisible. A request for a 16GB T4 must filter `gpu_ram>=16`, not `>=18`
// (16+headroom), or no T4 offer — every T4 ships 16GB — can ever match.
func TestEffectiveMemGB_T4Reachable(t *testing.T) {
	if got := EffectiveMemGB("t4", 16); got != 16 {
		t.Errorf("EffectiveMemGB(\"t4\", 16) = %d, want 16 (exact ceiling, no headroom)", got)
	}
	if got := EffectiveMemGB("T4", 16); got != 16 {
		t.Errorf("EffectiveMemGB(\"T4\", 16) = %d, want 16", got)
	}
	if !KnownHardwareMemoryGB("t4", 16) {
		t.Error("KnownHardwareMemoryGB(\"t4\", 16) = false, want true")
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
