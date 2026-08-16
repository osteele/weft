package db

import (
	"fmt"
	"time"
)

// Threshold is a survival-analysis threshold together with where it came from.
//
// The two are inseparable because reading one without the other is the bug this
// type exists to prevent. Every Compute*Survival returns a populated threshold
// whether or not a curve was fitted — on thin history it carries a configured
// default — so a bare duration cannot tell a caller whether it is looking at a
// measurement or a fallback. Watchdogs that killed instances on the strength of
// a default silently replaced their own tuned constants with it.
//
// Ask for what you actually need:
//
//   - Learned, for anything that decides to terminate, cordon, or otherwise act
//     destructively. It reports nothing when the threshold is a default, which
//     leaves the caller on its own constant.
//   - Duration and IsLearned, for display and diagnostics, which legitimately
//     want the effective value alongside a provenance label.
//
// The zero Threshold is "nothing known": Learned reports false and Duration
// returns zero. Non-positive durations are rejected at construction, so a
// Threshold can never carry a zero deadline that a `elapsed >= threshold`
// comparison would treat as already expired.
type Threshold struct {
	d       time.Duration
	learned bool
}

// LearnedThreshold builds a threshold fitted from an observed survival curve.
// A non-positive duration is not a fitted value and yields the zero Threshold.
func LearnedThreshold(d time.Duration) Threshold {
	if d <= 0 {
		return Threshold{}
	}
	return Threshold{d: d, learned: true}
}

// DefaultThreshold builds a threshold from configuration rather than data.
// A non-positive duration yields the zero Threshold.
func DefaultThreshold(d time.Duration) Threshold {
	if d <= 0 {
		return Threshold{}
	}
	return Threshold{d: d}
}

// Learned returns the threshold only when it was fitted from observed data.
// Enforcement paths call this; a default must not decide to kill anything.
func (t Threshold) Learned() (time.Duration, bool) {
	if !t.learned {
		return 0, false
	}
	return t.d, true
}

// Duration returns the effective threshold, learned or default, and zero when
// nothing is known. For display and diagnostics; pair it with IsLearned so the
// reader can tell which they are looking at.
func (t Threshold) Duration() time.Duration {
	return t.d
}

// IsLearned reports whether the threshold was fitted from observed data.
func (t Threshold) IsLearned() bool {
	return t.learned
}

// Known reports whether any threshold is set at all.
func (t Threshold) Known() bool {
	return t.d > 0
}

// String renders the value with its provenance, for logs and debug output.
func (t Threshold) String() string {
	if !t.Known() {
		return "unset"
	}
	if t.learned {
		return fmt.Sprintf("%s (learned)", t.d.Truncate(time.Second))
	}
	return fmt.Sprintf("%s (default)", t.d.Truncate(time.Second))
}
