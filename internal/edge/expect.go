package edge

import (
	"fmt"
	"sort"
	"time"
)

// Regime names what kind of claim a progress line's statistics are making.
//
// The three are reported differently on purpose. Printing four samples in the
// costume of a distribution invites a reader to trust a number that has not
// earned it, and an agent that mistrusts one statistic tends to mistrust the
// verdict attached to it.
type Regime string

const (
	// RegimeDeclared has no samples: the numbers come from configuration and
	// are an assertion about what someone expected, not a measurement.
	RegimeDeclared Regime = "declared"
	// RegimeProvisional has too few samples to describe a distribution, so the
	// observed range is reported with its sample count attached.
	RegimeProvisional Regime = "provisional"
	// RegimeMeasured has enough samples for percentiles to mean something.
	RegimeMeasured Regime = "measured"
)

// MeasuredThreshold is the sample count at which percentiles are reported.
const MeasuredThreshold = 10

// Expectation is what the hub knows about how long this class of work takes,
// and when a waiter should stop waiting.
//
// The two triggers are configured constants in every regime. Statistics only
// sharpen "is this normal"; the constants answer "when do I act", so agent
// behavior does not drift as history accumulates.
type Expectation struct {
	Class    string
	Regime   Regime
	Samples  []time.Duration
	Typical  time.Duration
	Escalate time.Duration
	Stall    time.Duration
}

// NewExpectation builds an expectation from configuration and observed history.
func NewExpectation(class string, typical, escalate, stall time.Duration, samples []time.Duration) *Expectation {
	e := &Expectation{
		Class:    class,
		Samples:  append([]time.Duration(nil), samples...),
		Typical:  typical,
		Escalate: escalate,
		Stall:    stall,
	}
	sort.Slice(e.Samples, func(i, j int) bool { return e.Samples[i] < e.Samples[j] })
	switch {
	case len(e.Samples) == 0:
		e.Regime = RegimeDeclared
	case len(e.Samples) < MeasuredThreshold:
		e.Regime = RegimeProvisional
	default:
		e.Regime = RegimeMeasured
	}
	return e
}

// StatisticsLine describes the expectation with its provenance attached, so a
// reader can tell a configured guess from a measurement.
func (e *Expectation) StatisticsLine() string {
	switch e.Regime {
	case RegimeDeclared:
		return fmt.Sprintf("typically ~%s (a declared envelope, not a measurement; no samples yet)",
			round(e.Typical))
	case RegimeProvisional:
		return fmt.Sprintf("observed %s to %s across %d run(s) (provisional, too few samples for a distribution)",
			round(e.Samples[0]), round(e.Samples[len(e.Samples)-1]), len(e.Samples))
	default:
		return fmt.Sprintf("p50 %s, p90 %s across %d runs",
			round(e.percentile(0.50)), round(e.percentile(0.90)), len(e.Samples))
	}
}

func (e *Expectation) percentile(p float64) time.Duration {
	if len(e.Samples) == 0 {
		return 0
	}
	idx := int(p * float64(len(e.Samples)-1))
	return e.Samples[idx]
}

// Verdict is the explicit normality judgment a progress line carries. A bare
// duration invites a reader to invent an interpretation; this states one.
type Verdict struct {
	Escalate bool
	Text     string
}

// Judge decides whether elapsed time and phase age warrant action.
//
// Both triggers are checked, and the stall trigger exists because the elapsed
// ceiling alone is too slow: a wedged submission at hour seven looks identical
// to a healthy long one until the ceiling expires much later.
func (e *Expectation) Judge(elapsed, phaseAge time.Duration) Verdict {
	if e.Stall > 0 && phaseAge >= e.Stall {
		return Verdict{
			Escalate: true,
			Text: fmt.Sprintf("ESCALATE — phase unchanged for %s, past the %s stall trigger",
				round(phaseAge), round(e.Stall)),
		}
	}
	if e.Escalate > 0 && elapsed >= e.Escalate {
		return Verdict{
			Escalate: true,
			Text: fmt.Sprintf("ESCALATE — elapsed %s, past the %s ceiling",
				round(elapsed), round(e.Escalate)),
		}
	}
	return Verdict{Text: "within expectation — no action indicated"}
}

// Progress renders one progress report: elapsed, phase, statistics with
// provenance, an explicit verdict, and the trigger that would change it.
//
// All five are present in every regime, including the one with no data. A
// report that omits the verdict is the failure this exists to prevent: an agent
// reading a bare elapsed time invents a story about it.
func (e *Expectation) Progress(elapsed, phaseAge time.Duration, phase string) string {
	if phase == "" {
		phase = "unknown"
	}
	verdict := e.Judge(elapsed, phaseAge)
	return fmt.Sprintf(
		"elapsed %s | phase %s (%s) | %s | %s | escalate at %s elapsed or %s in one phase",
		round(elapsed), phase, round(phaseAge), e.StatisticsLine(), verdict.Text,
		round(e.Escalate), round(e.Stall))
}

func round(d time.Duration) string {
	switch {
	case d == 0:
		return "unset"
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Minute).String()
	}
}

// DefaultExpectation is used for a class with no configured envelope.
func DefaultExpectation(class string, samples []time.Duration) *Expectation {
	return NewExpectation(class, 5*time.Minute, 2*time.Hour, 6*time.Hour, samples)
}
