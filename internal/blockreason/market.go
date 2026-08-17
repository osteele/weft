package blockreason

import (
	"regexp"
	"strings"

	"github.com/osteele/weft/internal/campaign"
)

// Why a blocked job might become launchable again, from the autopilot
// dispatcher's point of view.
//
// The dispatcher wakes on observed database changes and otherwise stays quiet.
// That is only safe for a blocker whose clearing event is itself a database
// change the wake snapshot observes. A blocker that clears because the rental
// market moved produces no local write at all, so suppressing timer-driven
// passes for it would strand the job until something unrelated happened.
//
// The classification is therefore one-sided on purpose: a reason counts as
// database-observable only when it is recognized as such. Anything
// unrecognized keeps the timer, which is the pre-classification behavior. See
// the "Absence of Evidence Is Not Evidence of Absence" rule in CLAUDE.md —
// failing to classify a reason is not evidence that the market is irrelevant
// to it.

// RecheckNeed says what has to happen before a blocked job could get a
// different answer. The three values are ordered by how much the dispatcher
// has to do about them, so the need of a set of reasons is the maximum.
type RecheckNeed int

const (
	// RecheckNone: a local write announces the change, and the wake snapshot
	// observes it. The runner can wait indefinitely for that write.
	RecheckNone RecheckNeed = iota

	// RecheckDeadline: a retry backoff has to elapse. The deadline is local
	// and near — the schedule in internal/retrypolicy tops out at a couple of
	// minutes — but nothing is written when it passes, so only a timer finds
	// it. Worth a slower recheck than the market, not an indefinite wait.
	RecheckDeadline

	// RecheckMarket: only a fresh provider query can resolve it. Offers appear
	// and vanish on the provider's schedule with no local signal at all, so
	// this is the one that earns the short cooldown.
	RecheckMarket
)

// Recheck classifies one blocked reason. Composite reasons (semicolon-joined)
// take the strongest need among their parts.
func Recheck(reason string) RecheckNeed {
	reason = strings.TrimSpace(campaign.SanitizeBlockedReason(reason))
	if reason == "" {
		return RecheckNone
	}
	// Evaluated across the whole reason rather than per part: a countdown often
	// arrives nested inside a reuse summary ("could not reuse running
	// instances: reuse backoff 45s remaining"), where the enclosing part is
	// database-observable but the operative gate is a clock.
	need := RecheckNone
	if hasBackoffCountdown(reason) {
		need = RecheckDeadline
	}
	recognized := false
	for _, part := range strings.Split(reason, "; ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// A bare countdown ("retry backoff 1m30s remaining (after 3
		// failure(s))") is recognized on its own; it names a clock, not an
		// unclassified blocker.
		if hasBackoffCountdown(part) || isDatabaseObservablePart(part) {
			recognized = true
			continue
		}
		return RecheckMarket
	}
	if !recognized {
		return RecheckMarket
	}
	return need
}

// RecheckFor returns the strongest need across a pass's blocked reasons.
// Reasons that render as waiting or paused rather than blocked are ignored,
// matching AutoPilotBlockedReasonCount: a pass whose only reasons are
// waiting-kind is not a blocked pass.
func RecheckFor(reasons map[int64]string) RecheckNeed {
	strongest := RecheckNone
	for _, reason := range reasons {
		if strings.TrimSpace(reason) == "" {
			continue
		}
		if ReasonKind(reason) != KindBlocked {
			continue
		}
		if need := Recheck(reason); need > strongest {
			strongest = need
		}
	}
	return strongest
}

// backoffCountdown matches the retry-countdown fragment shared by the relaunch,
// reuse, and submit backoff paths: "backoff 45s remaining (after 2 failure(s))".
// The deadline it names passes silently — no row is written when a backoff
// expires — so only a timer-driven pass can discover that it has.
var backoffCountdown = regexp.MustCompile(`backoff \S+ remaining`)

func hasBackoffCountdown(reason string) bool {
	return backoffCountdown.MatchString(reason)
}

// isDatabaseObservablePart reports whether one reason fragment names a blocker
// whose clearing event lands in the local database or config file. Each entry
// must name what clears it; a fragment nobody can explain that way belongs
// outside this list.
func isDatabaseObservablePart(part string) bool {
	switch {
	// The run-rate gate. Clears when a live launch ends (the launches table,
	// which the wake snapshot reads as RunRateCentsPerHour) or when the user
	// edits the hourly target (the config file, which the change source
	// watches).
	case IsRunRateBudgetReason(part):
		return true

	// The run-rate gate's launch-side verdict on the prefer-reuse retry path,
	// not a statement about offer availability.
	case strings.HasPrefix(part, NoRentalHeadroomHeadline):
		return true

	// Why running instances refused the job. Clears when an instance frees
	// capacity or a job completes — both lifecycle events. The headline
	// fragment is the summary form the autopilot appends to a launch-side
	// reason ("<launch>; running instances couldn't accept this job"), so a
	// budget verdict carrying it must stay database-observable.
	case isReuseDiagnosticPart(part),
		part == ReuseRejectedHeadline:
		return true
	}
	return false
}

// ReuseRejectedHeadline is the summary fragment appended to a launch-side
// blocker when running instances also refused the job, composing
// "<launch reason>; running instances couldn't accept this job".
const ReuseRejectedHeadline = "running instances couldn't accept this job"

// NoRentalHeadroomHeadline is the launch-side blocker the prefer-reuse retry
// path emits when the run-rate gate leaves no room to launch. It lives here
// rather than at its emitting site so the producer and this classifier cannot
// word it differently.
const NoRentalHeadroomHeadline = "no rental headroom"

// IsRunRateBudgetReason reports whether a reason is the run-rate (budget)
// gate's verdict — the launch-side blocker emitted when launching new
// instances would breach the configured run-rate target. It matches the
// "no subset fits", "headroom exhausted", and auto-launch variants.
func IsRunRateBudgetReason(reason string) bool {
	return strings.Contains(reason, "run-rate target exceeded") ||
		strings.Contains(reason, "run-rate headroom exhausted")
}
