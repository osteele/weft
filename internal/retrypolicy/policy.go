// Package retrypolicy defines the per-job relaunch backoff schedule.
package retrypolicy

import "time"

var backoffDelays = []time.Duration{
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
	2 * time.Minute,
}

func BackoffDelay(attempt int) (time.Duration, bool) {
	if attempt < 0 || attempt >= len(backoffDelays) {
		return 0, false
	}
	return backoffDelays[attempt], true
}

func BackoffDelayClamped(attempt int) time.Duration {
	switch {
	case len(backoffDelays) == 0:
		return 0
	case attempt < 0:
		return backoffDelays[0]
	case attempt >= len(backoffDelays):
		return backoffDelays[len(backoffDelays)-1]
	default:
		return backoffDelays[attempt]
	}
}

func MaxPlacementAttempts() int {
	return len(backoffDelays) + 1
}

// BackoffRemaining returns how long a caller must still wait, given count
// consecutive prior failures, the time of the most recent failure, and now.
// Zero means eligible. A zero/unknown last-failure time also yields zero —
// better a redundant retry than stalling the work indefinitely. Shared by
// the relaunch and reuse backoff paths so the window arithmetic cannot
// drift between them.
func BackoffRemaining(count int, last, now time.Time) time.Duration {
	if count <= 0 || last.IsZero() || last.Unix() <= 0 {
		return 0
	}
	delay := BackoffDelayClamped(count - 1)
	elapsed := now.Sub(last)
	if elapsed >= delay {
		return 0
	}
	return delay - elapsed
}

func MaxAttempts() int {
	return MaxPlacementAttempts()
}

func MaxAttemptsWithExtra(extra int) int {
	maxAttempts := MaxPlacementAttempts() + extra
	if maxAttempts < 1 {
		return 1
	}
	return maxAttempts
}

// MaxCreateAttempts is the total number of provider CreateInstance calls a
// single LaunchInstance may issue before giving up on the group. Each attempt
// may target a different offer — and with cross-provider fallback, a
// different provider — selected by the replacement-offer search. 4 is the
// historical default; it has not been calibrated against real provider
// flakiness, so revisit it if launch failure rates on flaky days demand
// more headroom.
func MaxCreateAttempts() int { return 4 }

// MaxReplacementOfferSearchAttempts caps how many candidate offers the
// replacement-offer search inspects per retry attempt before falling back
// to the price-cap rejection error. The cap is generous because listing and
// scoring offers is cheap relative to creating instances; 10 is the
// historical default.
func MaxReplacementOfferSearchAttempts() int { return 10 }

// MaxGroupReplans is the number of times the per-group launch goroutine
// re-fetches a fresh initial offer and re-runs the create-with-replacement
// chain after a previous chain failed retryably. 1 means: original chain
// plus one fresh chain. Replans exist because provider offer pools turn
// over within the move window — a freshly searched offer can succeed where
// the previous chain's accumulated exclude set could not.
func MaxGroupReplans() int { return 1 }
