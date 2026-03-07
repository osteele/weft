// Package estimate consolidates duration and cost estimation logic for cloud
// job lifecycle phases: instance startup, provisioning (sync + download), and
// job runtime.
package estimate

import "time"

// Estimate is a duration with uncertainty bounds (p10 lower, p90 upper).
type Estimate struct {
	Mean  time.Duration
	Lower time.Duration
	Upper time.Duration
}

// Breakdown holds estimates for each phase of a cloud job lifecycle.
type Breakdown struct {
	Startup   Estimate
	Provision Estimate
	Run       Estimate
	Total     Estimate
	// HasRunPrediction is true when Run came from the ML predictor rather than a fallback.
	HasRunPrediction bool
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

// Add combines two independent estimates by summing each bound.
func (e Estimate) Add(other Estimate) Estimate {
	return Estimate{
		Mean:  e.Mean + other.Mean,
		Lower: e.Lower + other.Lower,
		Upper: e.Upper + other.Upper,
	}
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

// DurFromMinutes converts a floating-point minute value to a time.Duration.
func DurFromMinutes(min float64) time.Duration {
	return time.Duration(min * float64(time.Minute))
}

// TransferTime estimates how long it takes to transfer sizeBytes at bandwidthBps
// (bytes per second). Returns an estimate with 0.5x/2.0x bounds for bandwidth variability.
func TransferTime(sizeBytes int64, bandwidthBps float64) Estimate {
	if sizeBytes <= 0 || bandwidthBps <= 0 {
		return Estimate{}
	}
	meanSec := float64(sizeBytes) / bandwidthBps
	return FromSeconds(meanSec, meanSec/2.0, meanSec*2.0)
}
