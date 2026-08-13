package vastai

import "testing"

// TestStoredMemFloorGB_HardwareCeilingExact is the regression for wj2265 and
// peers: a user request for "A100 80GB" must produce a filter of
// `gpu_ram>=80` (matches the hardware's reported 80GB), not `>=82` (which
// excludes the exact GPU they asked for). Submission-time headroom is
// skipped when the request hits a known ceiling.
func TestStoredMemFloorGB_HardwareCeilingExact(t *testing.T) {
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
		if got := StoredMemFloorGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("StoredMemFloorGB(%q, %d) = %d, want %d (exact ceiling)", tc.class, tc.mem, got, tc.want)
		}
	}
}

// TestStoredMemFloorGB_RollbackPostHeadroom is the back-compat path for jobs
// persisted before hardware-ceiling headroom was skipped: a stored value of
// (ceiling + defaultHeadroomGB) is recognized as the user's intent of
// `ceiling`. Without this, wj2265 (stored 82 from --gpu-class=a100
// --gpu-mem=80) would never find A100 80GB offers even after a fix to
// submission, because the DB value already had the headroom baked in.
func TestStoredMemFloorGB_RollbackPostHeadroom(t *testing.T) {
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
		if got := StoredMemFloorGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("StoredMemFloorGB(%q, %d) = %d, want %d (rollback)", tc.class, tc.mem, got, tc.want)
		}
	}
}

// TestStoredMemFloorGB_EffectiveFloorPassesThrough verifies that stored
// effective floors do not receive a second round of headroom.
func TestStoredMemFloorGB_EffectiveFloorPassesThrough(t *testing.T) {
	cases := []struct {
		class string
		mem   int
		want  int
	}{
		{"a100", 52, 52},
		{"a100", 62, 62},
		{"a100", 102, 102},
		{"l4", 22, 22},
	}
	for _, tc := range cases {
		if got := StoredMemFloorGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("StoredMemFloorGB(%q, %d) = %d, want %d", tc.class, tc.mem, got, tc.want)
		}
	}
}

// TestStoredMemFloorGB_UnknownClassPassesThrough verifies that effective
// floors remain stable even when the class cannot support ceiling rollback.
func TestStoredMemFloorGB_UnknownClassPassesThrough(t *testing.T) {
	cases := []struct {
		class string
		mem   int
		want  int
	}{
		{"", 26, 26},
		{"nvidia", 26, 26},
		{"unknown-model", 26, 26},
	}
	for _, tc := range cases {
		if got := StoredMemFloorGB(tc.class, tc.mem); got != tc.want {
			t.Errorf("StoredMemFloorGB(%q, %d) = %d, want %d", tc.class, tc.mem, got, tc.want)
		}
	}
}

// TestStoredMemFloorGB_NonPositiveIsPassthrough verifies that 0 and negative
// values aren't perturbed — they're a no-constraint signal upstream.
func TestStoredMemFloorGB_NonPositiveIsPassthrough(t *testing.T) {
	if got := StoredMemFloorGB("a100", 0); got != 0 {
		t.Errorf("StoredMemFloorGB(a100, 0) = %d, want 0", got)
	}
}

// TestStoredMemFloorGB_T4Reachable is the regression for wj3135: the user-facing
// class "t4" normalizes to "t4", but the table previously keyed the Tesla T4
// only under "teslat4" (the gpu_name normalization), so its 16GB ceiling was
// invisible. A request for a 16GB T4 must filter `gpu_ram>=16`, not `>=18`
// (16+headroom), or no T4 offer — every T4 ships 16GB — can ever match.
func TestStoredMemFloorGB_T4Reachable(t *testing.T) {
	if got := StoredMemFloorGB("t4", 16); got != 16 {
		t.Errorf("StoredMemFloorGB(\"t4\", 16) = %d, want 16 (exact ceiling, no headroom)", got)
	}
	if got := StoredMemFloorGB("T4", 16); got != 16 {
		t.Errorf("StoredMemFloorGB(\"T4\", 16) = %d, want 16", got)
	}
}
