package blockkind

import (
	"strings"
	"testing"
)

func TestReasonKindTaxonomy(t *testing.T) {
	cases := []struct {
		reason string
		want   Kind
	}{
		{"planner: offer fetch unavailable", KindWaiting},
		{"inventory-tagged: waiting for on-prem host", KindWaiting},
		{"offer unavailable: pod create --gpu-id: requested instance type is no longer available; Weft will retry with fresh offers", KindWaiting},
		{"provider rejected request (vastai create-instance) (contract 43292019); Weft will retry with fresh offers", KindWaiting},
		{`waiting for "output/model.pt" from wj1570 (queued)`, KindWaiting},
		{`waiting for "output/model.pt" from wj1570 (running)`, KindWaiting},
		{`waiting for "output/model.pt" from wj1570 (running); could not reuse wi42: image incompatible`, KindWaiting},
		{`waiting for "output/model.pt" from wj1570 (failed)`, KindBlocked},
		{`waiting for "output/model.pt" from wj1570 (producer not found)`, KindBlocked},
		{`waiting for "output/model.pt" from wj1570 (on-prem, not in R2)`, KindBlocked},
		{"planner: search offers: provider rejected request: 400 invalid filter", KindBlocked},
		{"new-instance retry blocked: paused: repeated launch failures without progress", KindPaused},
		{"gpu gate: no GPU with 26GB free", KindBlocked},
		{"", KindNone},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			if got := ReasonKind(tc.reason); got != tc.want {
				t.Fatalf("ReasonKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeOfferFetchUnavailable(t *testing.T) {
	got := NormalizeOfferFetchUnavailable("planner: offer fetch unavailable: cached offer snapshot missing")
	for _, want := range []string{
		"planner: provider offer fetch unavailable",
		"cached offer snapshot missing",
		"market unknown",
		"Weft will retry",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("NormalizeOfferFetchUnavailable = %q, want %q", got, want)
		}
	}
}
