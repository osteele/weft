package blockreason

import "testing"

// Reason strings as the planner and autopilot actually emit them. Keeping the
// real text here is the point of the test: the classifier matches on wording,
// so a paraphrase in the test would prove nothing about production behavior.
const (
	reasonNoOffers      = "no offers from providers for gpu=A40 vram>=20GB disk>=186GB"
	reasonOfferFetch    = "provider offer fetch unavailable (market unknown; Weft will retry)"
	reasonRunRateSubset = "run-rate target exceeded (no subset fits): target $2.00/hr, current $1.80/hr + requested $0.50/hr = $2.30/hr (headroom $0.20/hr, cheapest group $0.50/hr)"
	reasonRunRateSingle = "run-rate target exceeded: target $2.00/hr, current $1.80/hr + requested $0.50/hr = $2.30/hr (headroom $0.20/hr, job needs $0.50/hr)"
	reasonHeadroomGone  = "run-rate headroom exhausted ($0.20/hr free, this group needs $1.50/hr)"
	reasonNoRental      = "no rental headroom; running instances couldn't accept this job"
	reasonReuse         = "could not reuse running instances: wi12 has no free GPU slots"
	reasonInventory     = "inventory-tagged: waiting for on-prem host"

	// Retry countdowns, as emitted by the reuse, submit, and relaunch backoff
	// paths. Nothing is written when one of these deadlines passes, so only a
	// timer-driven pass can notice that it has.
	reasonReuseBackoff  = "reuse backoff 45s remaining (after 2 failed submit(s))"
	reasonRetryBackoff  = "retry backoff 1m30s remaining (after 3 failure(s))"
	reasonNestedBackoff = "could not reuse running instances: reuse backoff 45s remaining (after 2 failed submit(s))"
)

func TestRecheck(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   RecheckNeed
	}{
		{name: "empty", reason: "", want: RecheckNone},
		{name: "no offers needs the market", reason: reasonNoOffers, want: RecheckMarket},
		{name: "offer fetch failure needs the market", reason: reasonOfferFetch, want: RecheckMarket},
		{name: "retryable offer needs the market", reason: "offer unavailable: requested instance type is no longer available; Weft will retry with fresh offers", want: RecheckMarket},
		{name: "run-rate subset is database-observable", reason: reasonRunRateSubset, want: RecheckNone},
		{name: "run-rate single job is database-observable", reason: reasonRunRateSingle, want: RecheckNone},
		{name: "headroom exhausted is database-observable", reason: reasonHeadroomGone, want: RecheckNone},
		{name: "no rental headroom is the budget gate, not the market", reason: reasonNoRental, want: RecheckNone},
		{name: "reuse rejection is database-observable", reason: reasonReuse, want: RecheckNone},
		{
			name:   "budget reason with reuse detail stays database-observable",
			reason: reasonRunRateSingle + "; " + reasonReuse,
			want:   RecheckNone,
		},
		{
			name:   "any market part makes the whole reason market-sensitive",
			reason: reasonRunRateSingle + "; " + reasonNoOffers,
			want:   RecheckMarket,
		},
		{
			name:   "unrecognized reason is treated as market-sensitive",
			reason: "some blocker nobody has classified",
			want:   RecheckMarket,
		},
		{
			// A backoff expires on a clock, not on a write. Classifying it as
			// database-observable would leave the job waiting for the backstop
			// instead of the cooldown.
			name:   "reuse backoff countdown waits on a deadline",
			reason: reasonReuseBackoff,
			want:   RecheckDeadline,
		},
		{
			name:   "retry backoff countdown waits on a deadline",
			reason: reasonRetryBackoff,
			want:   RecheckDeadline,
		},
		{
			// The countdown is nested inside an otherwise database-observable
			// reuse summary, so a per-part check would miss it.
			name:   "countdown nested in a reuse summary waits on a deadline",
			reason: reasonNestedBackoff,
			want:   RecheckDeadline,
		},
		{
			name:   "countdown alongside a budget verdict waits on a deadline",
			reason: reasonRunRateSingle + "; " + reasonReuseBackoff,
			want:   RecheckDeadline,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Recheck(tc.reason); got != tc.want {
				t.Errorf("Recheck(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

func TestRecheckFor(t *testing.T) {
	tests := []struct {
		name    string
		reasons map[int64]string
		want    RecheckNeed
	}{
		{name: "nil map", reasons: nil, want: RecheckNone},
		{name: "blank reasons", reasons: map[int64]string{1: "", 2: "   "}, want: RecheckNone},
		{
			name:    "budget only",
			reasons: map[int64]string{1: reasonRunRateSingle, 2: reasonReuse},
			want:    RecheckNone,
		},
		{
			name:    "one market blocker among budget blockers",
			reasons: map[int64]string{1: reasonRunRateSingle, 2: reasonNoOffers},
			want:    RecheckMarket,
		},
		{
			// Waiting-kind reasons do not make a pass blocked, so they must
			// not drag the timer back on either. Inventory jobs wait for an
			// on-prem host, which is a lifecycle event, not a market move.
			name:    "waiting-kind reasons are ignored",
			reasons: map[int64]string{1: reasonInventory},
			want:    RecheckNone,
		},
		{
			// The offer-fetch reason renders as waiting so the user is not
			// told the job is stuck, but it is still only clearable by asking
			// the provider again. Confirm which side of the line it falls on.
			name:    "offer fetch failure renders as waiting",
			reasons: map[int64]string{1: reasonOfferFetch},
			want:    marketIfBlockedKind(reasonOfferFetch),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecheckFor(tc.reasons); got != tc.want {
				t.Errorf("RecheckFor(%v) = %v, want %v", tc.reasons, got, tc.want)
			}
		})
	}
}

// Every reason the classifier calls database-observable must be one the
// autopilot's blocked-reason accounting actually treats as blocked. A reason
// that renders as waiting never reaches the timer decision, so listing it
// would be misleading rather than wrong.
func TestDatabaseObservableReasonsAreBlockedKind(t *testing.T) {
	for _, reason := range []string{
		reasonRunRateSubset,
		reasonRunRateSingle,
		reasonHeadroomGone,
		reasonNoRental,
	} {
		if kind := ReasonKind(reason); kind != KindBlocked {
			t.Errorf("ReasonKind(%q) = %q, want %q; a non-blocked reason never reaches the timer decision",
				reason, kind, KindBlocked)
		}
	}
}

// The clock-driven family must reach the timer decision at all, which means
// the autopilot has to count these reasons as blocked rather than waiting.
func TestBackoffCountdownsAreBlockedKind(t *testing.T) {
	for _, reason := range []string{reasonReuseBackoff, reasonRetryBackoff, reasonNestedBackoff} {
		if kind := ReasonKind(reason); kind != KindBlocked {
			t.Errorf("ReasonKind(%q) = %q, want %q", reason, kind, KindBlocked)
		}
		if got := RecheckFor(map[int64]string{1: reason}); got != RecheckDeadline {
			t.Errorf("RecheckFor(%q) = %v, want %v; a backoff expiry is announced by nothing", reason, got, RecheckDeadline)
		}
	}
}

// A market blocker outranks a deadline blocker: the set has to be rechecked at
// the shorter cadence, because the market half can change at any moment.
func TestRecheckForTakesTheStrongestNeed(t *testing.T) {
	got := RecheckFor(map[int64]string{1: reasonReuseBackoff, 2: reasonNoOffers})
	if got != RecheckMarket {
		t.Errorf("RecheckFor = %v, want %v", got, RecheckMarket)
	}
}

func marketIfBlockedKind(reason string) RecheckNeed {
	if ReasonKind(reason) == KindBlocked {
		return RecheckMarket
	}
	return RecheckNone
}

func TestIsRunRateBudgetReason(t *testing.T) {
	for _, reason := range []string{reasonRunRateSubset, reasonRunRateSingle, reasonHeadroomGone} {
		if !IsRunRateBudgetReason(reason) {
			t.Errorf("IsRunRateBudgetReason(%q) = false, want true", reason)
		}
	}
	for _, reason := range []string{reasonNoOffers, reasonReuse, ""} {
		if IsRunRateBudgetReason(reason) {
			t.Errorf("IsRunRateBudgetReason(%q) = true, want false", reason)
		}
	}
}
