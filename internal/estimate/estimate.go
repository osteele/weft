// Package estimate consolidates duration and cost estimation logic for cloud
// job lifecycle phases: instance startup, provisioning (sync + download), and
// job runtime.
package estimate

import (
	"fmt"
	"math"
	"time"
)

// Estimate is a duration with uncertainty bounds (p10 lower, p90 upper).
type Estimate struct {
	Mean  time.Duration
	Lower time.Duration
	Upper time.Duration
}

// Breakdown holds estimates for each phase of a cloud job lifecycle.
type Breakdown struct {
	Startup   Estimate
	SSHSetup  Estimate
	JobSetup  Estimate
	Provision Estimate
	Run       Estimate
	Upload    Estimate
	Total     Estimate
}

// Constant returns an Estimate with no uncertainty.
func Constant(d time.Duration) Estimate {
	return Estimate{Mean: d, Lower: d, Upper: d}
}

// FromSeconds builds an Estimate from seconds.
func FromSeconds(mean, lower, upper float64) Estimate {
	return Estimate{
		Mean:  time.Duration(mean * float64(time.Second)),
		Lower: time.Duration(lower * float64(time.Second)),
		Upper: time.Duration(upper * float64(time.Second)),
	}
}

// Add combines two independent estimates. Assumes the estimates represent
// independent random variables, so deviations are combined in quadrature.
func (e Estimate) Add(other Estimate) Estimate {
	mean := e.Mean + other.Mean
	loDev := math.Sqrt(sqrDur(e.Mean-e.Lower) + sqrDur(other.Mean-other.Lower))
	hiDev := math.Sqrt(sqrDur(e.Upper-e.Mean) + sqrDur(other.Upper-other.Mean))
	return Estimate{
		Mean:  mean,
		Lower: mean - time.Duration(loDev),
		Upper: mean + time.Duration(hiDev),
	}
}

func sqrDur(d time.Duration) float64 {
	f := float64(d)
	return f * f
}

// Scale multiplies all bounds by a constant factor.
func (e Estimate) Scale(factor float64) Estimate {
	return Estimate{
		Mean:  time.Duration(float64(e.Mean) * factor),
		Lower: time.Duration(float64(e.Lower) * factor),
		Upper: time.Duration(float64(e.Upper) * factor),
	}
}

// Zero returns true if the estimate has zero mean duration.
func (e Estimate) Zero() bool {
	return e.Mean == 0
}

// FormatWithBounds formats a duration estimate as "~2h15 (1h–4h)".
// Returns "—" for zero estimates, and omits bounds when they equal the mean.
func (e Estimate) FormatWithBounds() string {
	if e.Mean == 0 {
		return "—"
	}
	mean := FormatDurationShort(e.Mean)
	if e.Lower == e.Upper || e.Lower == e.Mean {
		return "~" + mean
	}
	lower := FormatDurationShort(e.Lower)
	upper := FormatDurationShort(e.Upper)
	return fmt.Sprintf("~%s (%s–%s)", mean, lower, upper)
}

// FormatDurationShort formats a duration compactly (e.g., "2h15", "45m", "30s").
func FormatDurationShort(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%02d", h, m)
}

// DurFromMinutes converts a floating-point minute value to a time.Duration.
func DurFromMinutes(min float64) time.Duration {
	return time.Duration(min * float64(time.Minute))
}

// TransferTime estimates how long it takes to transfer sizeBytes at bandwidthBps
// (bytes per second). Returns an estimate with 0.7x/1.5x bounds for bandwidth
// variability, based on empirical upload duration data (p10/p50 ≈ 0.17,
// p90/p50 ≈ 1.0; the bounds are kept slightly wider to cover download variance).
func TransferTime(sizeBytes int64, bandwidthBps float64) Estimate {
	if sizeBytes <= 0 || bandwidthBps <= 0 {
		return Estimate{}
	}
	meanSec := float64(sizeBytes) / bandwidthBps
	return FromSeconds(meanSec, meanSec*0.7, meanSec*1.5)
}
