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
	if len(axisEnforcement) == 0 {
		t.Fatal("no providers recorded")
	}
	for provider, byAxis := range axisEnforcement {
		for _, axis := range cloud.AllConstraintAxes() {
			if _, recorded := byAxis[axis]; !recorded {
				t.Errorf("provider %q does not record who checks constraint axis %q.\n"+
					"Add it with the filter that checks it, why the provider's own search "+
					"covers it, or why the provider has no such concept. A union over "+
					"providers is what let disk, cpu_cores, reliability and geo read as "+
					"enforced when only Vast.ai enforced them.", provider, axis)
			}
		}
	}
}

// Every provider weft can search must record its enforcement. A provider added
// without a map entry would inherit nothing and be silently unchecked, which is
// the failure this whole mechanism exists to make impossible.
func TestEveryProviderRecordsEnforcement(t *testing.T) {
	for _, provider := range []cloud.Provider{cloud.ProviderVastai, cloud.ProviderRunpod} {
		if _, ok := axisEnforcement[provider]; !ok {
			t.Errorf("provider %q has no axisEnforcement entry", provider)
		}
	}
}

// An axis no provider enforces and no local filter re-checks is the silent
// gap. notApplicable is only legitimate when the provider has no such concept;
// it must never be the answer everywhere at once, which would mean the
// constraint is accepted from users and enforced by nobody.
func TestNoAxisIsNotApplicableEverywhere(t *testing.T) {
	for _, axis := range cloud.AllConstraintAxes() {
		everywhere := true
		for _, byAxis := range axisEnforcement {
			if note, ok := byAxis[axis]; !ok || note.by != notApplicable {
				everywhere = false
				break
			}
		}
		if everywhere {
			t.Errorf("axis %q is notApplicable for every provider: it is accepted from "+
				"users and enforced by nobody. Either some provider supports it, or it "+
				"should not be in OfferConstraints.", axis)
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
