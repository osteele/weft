package cloud

import (
	"reflect"
	"slices"
	"testing"
)

// The tripwire. A new OfferConstraints field reaches placement whether or not
// anyone decided who enforces it, and an unenforced constraint is invisible:
// the request is accepted, offers come back, nothing rejects them, and the job
// runs on hardware it did not ask for. Requiring the tag is what turns that
// into a failure here.
func TestEveryConstraintFieldNamesItsAxis(t *testing.T) {
	if len(untaggedConstraintFields) > 0 {
		t.Fatalf("OfferConstraints fields with no `axis` tag: %v\n"+
			"Tag each with the axis it constrains (e.g. `axis:\"disk\"`), or with "+
			"`axis:\"-\"` if it constrains no offer property, then record who "+
			"enforces the axis in campaign.axisEnforcement.", untaggedConstraintFields)
	}
}

// The vocabulary is only as good as its correspondence to the declared axis
// constants: a typo in a struct tag would otherwise mint a new axis silently.
func TestDerivedAxesAreDeclaredConstants(t *testing.T) {
	declared := NewAxisSet(
		AxisGPUVariant, AxisGPUSKU, AxisGPUMemory, AxisNumGPUs, AxisDisk,
		AxisHostRAM, AxisCPUCores, AxisReliability, AxisDriver, AxisCUDA,
		AxisInterconnect, AxisGeo, AxisInstanceType, AxisCloudType,
	)
	for _, axis := range AllConstraintAxes() {
		if !declared.Has(axis) {
			t.Errorf("axis %q comes from an OfferConstraints tag but matches no Axis* constant", axis)
		}
	}
	if missing := declared.Missing(); len(missing) > 0 {
		t.Errorf("Axis* constants not reachable from any OfferConstraints tag: %v", missing)
	}
}

// GPUClass carries two axes because a class name can bind a package variant
// ("a100-sxm4") and an exact capacity ("a100-sxm4-80gb") independently, and
// providers can express the first but not the second.
func TestMultiAxisFieldYieldsEachAxis(t *testing.T) {
	axes := AllConstraintAxes()
	if !slices.Contains(axes, AxisGPUVariant) || !slices.Contains(axes, AxisGPUSKU) {
		t.Fatalf("AllConstraintAxes = %v, want both axes of the GPUClass tag", axes)
	}
}

func TestMissingReportsAbsentAxesInVocabularyOrder(t *testing.T) {
	got := NewAxisSet(AxisGPUVariant).Missing()
	want := slices.DeleteFunc(AllConstraintAxes(), func(a ConstraintAxis) bool {
		return a == AxisGPUVariant
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Missing() = %v, want %v", got, want)
	}
}
