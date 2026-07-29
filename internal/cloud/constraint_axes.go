package cloud

import (
	"reflect"
	"slices"
	"strings"
)

// ConstraintAxis names one independently-satisfiable dimension of an
// OfferConstraints.
//
// Providers differ in which axes their own search can express. Vast.ai filters
// most of them server-side but cannot express an exact GPU capacity, because
// its gpu_name field carries no memory component — "A100 SXM4" names both the
// 40GB and the 80GB part. RunPod filters client-side over a GPU-type list and
// expresses a different subset.
//
// An axis a provider cannot enforce is not thereby unconstrained: it has to be
// re-checked locally against the returned offers. The failure this vocabulary
// exists to prevent is the silent third case — an axis neither enforced nor
// re-checked, which is how jobs requesting 80GB parts were served 40GB ones.
type ConstraintAxis string

const (
	AxisGPUVariant   ConstraintAxis = "gpu_variant" // model and package variant, e.g. A100 SXM4 vs PCIe
	AxisGPUSKU       ConstraintAxis = "gpu_sku"     // exact capacity as part identity, e.g. the 80GB A100
	AxisGPUMemory    ConstraintAxis = "gpu_memory"  // per-GPU memory floor
	AxisNumGPUs      ConstraintAxis = "num_gpus"
	AxisDisk         ConstraintAxis = "disk"
	AxisHostRAM      ConstraintAxis = "host_ram"
	AxisCPUCores     ConstraintAxis = "cpu_cores"
	AxisReliability  ConstraintAxis = "reliability"
	AxisDriver       ConstraintAxis = "driver" // NVIDIA driver version floor
	AxisCUDA         ConstraintAxis = "cuda"   // provider CUDA compatibility floor
	AxisInterconnect ConstraintAxis = "interconnect"
	AxisGeo          ConstraintAxis = "geo"           // excluded countries
	AxisInstanceType ConstraintAxis = "instance_type" // on-demand vs interruptible
	AxisCloudType    ConstraintAxis = "cloud_type"    // RunPod community vs secure
)

// axisTag is the OfferConstraints struct tag naming the axes a field
// constrains.
const axisTag = "axis"

// constraintAxesFromTags reads the axis vocabulary off OfferConstraints, in
// field order, and reports any exported field carrying no axis tag.
//
// Deriving the vocabulary rather than hand-listing it is what gives the
// completeness check its teeth: a hand-written list cannot notice a field it
// was never told about, so the check it feeds would pass while the new
// constraint went unenforced. Reading the struct means a field must name its
// axis — or opt out with `axis:"-"` — before it can reach placement at all.
func constraintAxesFromTags() (axes []ConstraintAxis, untagged []string) {
	for _, field := range reflect.VisibleFields(reflect.TypeOf(OfferConstraints{})) {
		if !field.IsExported() {
			continue
		}
		tag, ok := field.Tag.Lookup(axisTag)
		if !ok {
			untagged = append(untagged, field.Name)
			continue
		}
		if tag == "-" {
			continue
		}
		for _, name := range strings.Split(tag, ",") {
			axis := ConstraintAxis(strings.TrimSpace(name))
			if axis != "" && !slices.Contains(axes, axis) {
				axes = append(axes, axis)
			}
		}
	}
	return axes, untagged
}

var allConstraintAxes, untaggedConstraintFields = constraintAxesFromTags()

// AllConstraintAxes is the complete axis vocabulary, in OfferConstraints field
// order. Every axis must be either enforced by the provider's own search or
// re-checked locally against the returned offers; the campaign package's
// coverage test asserts that nothing falls between the two.
func AllConstraintAxes() []ConstraintAxis { return slices.Clone(allConstraintAxes) }

// AxisSet is a set of constraint axes.
type AxisSet map[ConstraintAxis]bool

// NewAxisSet builds a set from a list.
func NewAxisSet(axes ...ConstraintAxis) AxisSet {
	set := make(AxisSet, len(axes))
	for _, a := range axes {
		set[a] = true
	}
	return set
}

// Has reports membership.
func (s AxisSet) Has(a ConstraintAxis) bool { return s[a] }

// Missing returns the axes in AllConstraintAxes absent from s, in vocabulary
// order.
func (s AxisSet) Missing() []ConstraintAxis {
	var missing []ConstraintAxis
	for _, a := range allConstraintAxes {
		if !s[a] {
			missing = append(missing, a)
		}
	}
	return missing
}
