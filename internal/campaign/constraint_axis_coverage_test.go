package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

// Every constraint axis must be either re-checked locally or enforced by the
// provider's own search. The third case — neither — is invisible: the request
// is accepted, offers come back, nothing rejects them, and the job runs on
// hardware it did not ask for. That is what happened to the exact-GPU capacity
// axis, unrepresentable in vast.ai's gpu_name filter and unchecked locally, so
// jobs requesting 80GB parts were served 40GB ones and reported "completed ok".
func TestConstraintAxisCoverage(t *testing.T) {
	for _, axis := range cloud.AllConstraintAxes() {
		if _, recorded := axisEnforcement[axis]; !recorded {
			t.Errorf("constraint axis %q is neither re-checked locally nor provider-enforced\n"+
				"Add it to axisEnforcement, with the filter that checks it or with "+
				"why weft cannot re-check it.", axis)
		}
	}
}

// The SKU axis is why this mechanism exists: vast.ai's gpu_name carries no
// memory component, so no provider filter can bind an exact capacity and the
// local re-check is the only thing standing between a request for an 80GB part
// and a 40GB card.
func TestSKUAxisIsRecheckedLocally(t *testing.T) {
	if !LocallyRecheckedAxes().Has(cloud.AxisGPUSKU) {
		t.Error("the SKU axis is unenforceable upstream and must be re-checked locally")
	}
}
