package placement

import "testing"

// A memory token inside a class names a SKU, not a floor: a 40GB A100 SXM4 is
// different hardware from the 80GB part, not a smaller instance of it. This
// axis previously went unchecked on every path — fresh cloud offers accepted
// any capacity, while on-prem and reuse rejected every candidate because the
// name matcher compared the token against part names that never carry one.
func TestGPUConstraint_SKUMemoryAxis(t *testing.T) {
	cases := []struct {
		name     string
		class    string
		deviceGB int
		want     bool
	}{
		// Drivers report usable memory below the nominal capacity, so the
		// comparison rounds to the SKU first.
		{"80GB part reporting usable memory", "a100-sxm4-80gb", 79, true},
		{"exact nominal", "a100-sxm4-80gb", 80, true},
		// The substitution this exists to catch: wj5503/5504/5509.
		{"40GB part against an 80GB request", "a100-sxm4-80gb", 39, false},
		{"40GB nominal against an 80GB request", "a100-sxm4-80gb", 40, false},
		// No token means the class is a variant selector, not a SKU.
		{"no token accepts any capacity", "a100-sxm4", 39, true},
		// Unknown is not disqualifying.
		{"unreported device memory", "a100-sxm4-80gb", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseGPUConstraint(tc.class)
			device := TargetDevice{Class: "A100 SXM4", Name: "A100 SXM4", MemoryGB: tc.deviceGB}
			if got := device.matchesGPUConstraint(c); got != tc.want {
				t.Errorf("matchesGPUConstraint(%q, %dGB) = %v, want %v", tc.class, tc.deviceGB, got, tc.want)
			}
		})
	}
}

// The name axis must be memory-agnostic now that memory is its own axis:
// a suffixed class has to reach the right part before its capacity is judged.
func TestGPUConstraint_SuffixedClassStillMatchesPartName(t *testing.T) {
	c := ParseGPUConstraint("a100-sxm4-80gb")
	if !c.MatchesGPU("A100 SXM4") {
		t.Error("suffixed class must match its part on the name axis")
	}
	if c.MatchesGPU("A100 PCIE") {
		t.Error("suffixed class must not match a different variant")
	}
}

// Moving the capacity off the name axis makes two different SKUs share a
// normalized name, so Subsumes has to consult the new axis too. If it does not,
// a 40GB job and an 80GB job merge into one rental and one of them is served
// the wrong hardware.
func TestGPUConstraint_SubsumesRespectsSKUMemoryAxis(t *testing.T) {
	cases := []struct {
		outer, inner string
		want         bool
	}{
		{"a100-sxm4-40gb", "a100-sxm4-80gb", false},
		{"a100-sxm4-80gb", "a100-sxm4-40gb", false},
		{"a100-sxm4-80gb", "a100-sxm4-80gb", true},
		// A class naming no capacity accepts every SKU of the part...
		{"a100-sxm4", "a100-sxm4-80gb", true},
		// ...but a class naming one is not satisfied by an unspecified request.
		{"a100-sxm4-80gb", "a100-sxm4", false},
	}
	for _, tc := range cases {
		outer, inner := ParseGPUConstraint(tc.outer), ParseGPUConstraint(tc.inner)
		if got := outer.Subsumes(inner); got != tc.want {
			t.Errorf("ParseGPUConstraint(%q).Subsumes(%q) = %v, want %v", tc.outer, tc.inner, got, tc.want)
		}
	}
}

// A class the catalogue has never heard of cannot have its SKU validated. Weft
// lets it through rather than rejecting it, because a stale catalogue is not
// evidence the hardware is wrong — the same rule the cloud offer filter uses,
// which is why both now ask gpucatalog rather than each deciding for itself.
func TestGPUConstraint_UncataloguedClassIsNotDisqualified(t *testing.T) {
	c := ParseGPUConstraint("some-future-gpu-96gb")
	device := TargetDevice{Class: "Some Future GPU", Name: "Some Future GPU", MemoryGB: 95}
	if !device.matchesGPUConstraint(c) {
		t.Error("an uncatalogued class must not be rejected on the SKU axis")
	}
}
