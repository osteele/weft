package db

import (
	"math"
	"strings"
)

// ThermalThrottleThresholdC is the GPU temperature (°C) above which
// thermal throttling is likely.
const ThermalThrottleThresholdC = 80

// Telemetry provenance values for GPUTelemetryStats.Source. A consumer
// pooling numbers across jobs needs to know how each was obtained: the rollup
// carries fewer fields than the per-sample paths, so a field missing from a
// rollup-sourced payload means "this path cannot report it", not "the GPU read
// zero". A merged payload names every contributing path joined by "+", so
// Source is matched by splitting on "+" rather than compared to one constant.
const (
	TelemetrySourceGPUSamples = "gpu_samples"
	TelemetrySourceTimeseries = "timeseries_samples"
	TelemetrySourceRollup     = "rollup"
)

// GPUTelemetryStats summarises GPU telemetry. Every measured field is a
// pointer: nil means no path reported it, and a non-nil zero is a real
// reading. A GPU genuinely idles at 0% utilisation, so a bare int cannot tell
// those apart, and a consumer that cannot tell will pool fabricated zeros into
// real measurements without noticing.
type GPUTelemetryStats struct {
	SampleCount int      `json:"sample_count"`
	Source      string   `json:"source"`
	TempMin     *int     `json:"temp_min_c,omitempty"`
	TempMax     *int     `json:"temp_max_c,omitempty"`
	TempMean    *float64 `json:"temp_mean_c,omitempty"`
	UtilMin     *int     `json:"util_min_pct,omitempty"`
	UtilMax     *int     `json:"util_max_pct,omitempty"`
	UtilMean    *float64 `json:"util_mean_pct,omitempty"`
	ClockMin    *int     `json:"clock_min_mhz,omitempty"`
	ClockMax    *int     `json:"clock_max_mhz,omitempty"`
	ClockMean   *float64 `json:"clock_mean_mhz,omitempty"`
	MemPeakMiB  *int     `json:"mem_peak_mib,omitempty"`
	MemTotalMiB *int     `json:"mem_total_mib,omitempty"`
	Throttled   *bool    `json:"throttled,omitempty"`
}

// running accumulates one metric so absence stays distinguishable from zero:
// a series nothing reported yields nil rather than a zero-valued triple.
type running struct {
	min, max float64
	sum      float64
	count    int
}

func (r *running) add(v float64) {
	if r.count == 0 || v < r.min {
		r.min = v
	}
	if r.count == 0 || v > r.max {
		r.max = v
	}
	r.sum += v
	r.count++
}

func (r running) minInt() *int {
	if r.count == 0 {
		return nil
	}
	v := int(r.min)
	return &v
}

func (r running) maxInt() *int {
	if r.count == 0 {
		return nil
	}
	v := int(r.max)
	return &v
}

func (r running) mean() *float64 {
	if r.count == 0 {
		return nil
	}
	v := math.Round(r.sum/float64(r.count)*10) / 10
	return &v
}

func telemetryInt(v int) *int           { return &v }
func telemetryFloat(v float64) *float64 { return &v }
func telemetryBool(v bool) *bool        { return &v }

// ComputeGPUTelemetryStatsFromGPUSamples summarises the per-device telemetry
// rows, which are the only source carrying GPU clocks. Utilisation and clocks
// are taken across every device in the sample, so a multi-GPU job reports the
// range its hardware actually spanned.
func ComputeGPUTelemetryStatsFromGPUSamples(samples []TelemetrySample) *GPUTelemetryStats {
	var util, clock, memUsed running
	for _, s := range samples {
		for _, g := range s.GPUs {
			if g.GPUUtilPct != nil {
				util.add(*g.GPUUtilPct)
			}
			if g.GPUSMClockMHz != nil {
				clock.add(float64(*g.GPUSMClockMHz))
			}
			if g.GPUMemUsedMiB > 0 {
				memUsed.add(float64(g.GPUMemUsedMiB))
			}
		}
	}
	if util.count == 0 && clock.count == 0 && memUsed.count == 0 {
		return nil
	}
	stats := &GPUTelemetryStats{
		SampleCount: len(samples),
		Source:      TelemetrySourceGPUSamples,
		UtilMin:     util.minInt(),
		UtilMax:     util.maxInt(),
		UtilMean:    util.mean(),
		ClockMin:    clock.minInt(),
		ClockMax:    clock.maxInt(),
		ClockMean:   clock.mean(),
		MemPeakMiB:  memUsed.maxInt(),
	}
	// The per-device rows carry no temperature, so throttling cannot be
	// judged from them and is left unreported rather than asserted false.
	return stats
}

// ComputeGPUTelemetryStats computes aggregate GPU statistics from timeseries
// samples. Returns nil if no sample reported any GPU metric.
func ComputeGPUTelemetryStats(samples []TimeseriesSample) *GPUTelemetryStats {
	var temp, util, clock, memUsed, memTotal running
	for _, s := range samples {
		if s.GPUTempC > 0 {
			temp.add(float64(s.GPUTempC))
		}
		if s.GPUUtilPct > 0 {
			util.add(float64(s.GPUUtilPct))
		}
		if s.GPUClockMHz > 0 {
			clock.add(float64(s.GPUClockMHz))
		}
		if s.GPUMemUsedMiB > 0 {
			memUsed.add(float64(s.GPUMemUsedMiB))
		}
		if s.GPUMemTotalMiB > 0 {
			memTotal.add(float64(s.GPUMemTotalMiB))
		}
	}
	if temp.count+util.count+clock.count == 0 {
		return nil
	}
	stats := &GPUTelemetryStats{
		SampleCount: max(temp.count, max(util.count, clock.count)),
		Source:      TelemetrySourceTimeseries,
		TempMin:     temp.minInt(),
		TempMax:     temp.maxInt(),
		TempMean:    temp.mean(),
		UtilMin:     util.minInt(),
		UtilMax:     util.maxInt(),
		UtilMean:    util.mean(),
		ClockMin:    clock.minInt(),
		ClockMax:    clock.maxInt(),
		ClockMean:   clock.mean(),
		MemPeakMiB:  memUsed.maxInt(),
		MemTotalMiB: memTotal.maxInt(),
	}
	if temp.count > 0 {
		stats.Throttled = telemetryBool(temp.max > ThermalThrottleThresholdC)
	}
	return stats
}

// GPUTelemetryStatsFromTimeseriesSummary adapts the pre-aggregated rollup.
// The rollup stores no minima, no clocks, and no total memory, so those stay
// nil: this path cannot report them, which is a different claim from having
// measured zero.
func GPUTelemetryStatsFromTimeseriesSummary(summary *TimeseriesSummary) *GPUTelemetryStats {
	if summary == nil || summary.SampleCount == 0 {
		return nil
	}
	if summary.PeakGPUTempC == 0 && summary.PeakGPUUtilPct == 0 && summary.PeakGPUMemMiB == 0 {
		return nil
	}
	stats := &GPUTelemetryStats{
		SampleCount: summary.SampleCount,
		Source:      TelemetrySourceRollup,
	}
	if summary.PeakGPUTempC > 0 {
		stats.TempMax = telemetryInt(summary.PeakGPUTempC)
		stats.TempMean = telemetryFloat(summary.MeanGPUTempC)
		stats.Throttled = telemetryBool(summary.PeakGPUTempC > ThermalThrottleThresholdC)
	}
	if summary.PeakGPUUtilPct > 0 {
		stats.UtilMax = telemetryInt(summary.PeakGPUUtilPct)
		stats.UtilMean = telemetryFloat(summary.MeanGPUUtilPct)
	}
	if summary.PeakGPUMemMiB > 0 {
		stats.MemPeakMiB = telemetryInt(summary.PeakGPUMemMiB)
	}
	return stats
}

// MergeGPUTelemetryStats fills each field from the first candidate that
// reported it, in the order given. No single source is complete: the
// per-device rows carry GPU clocks but no temperature, while the timeseries
// samples carry temperature and total memory but only one aggregate clock.
// Preferring either wholesale trades one blind spot for another, so each field
// is taken from the best source that actually has it.
//
// Source names every contributor, joined by "+", so a consumer can still tell
// how a given payload was assembled.
func MergeGPUTelemetryStats(candidates ...*GPUTelemetryStats) *GPUTelemetryStats {
	var merged *GPUTelemetryStats
	var sources []string
	for _, c := range candidates {
		if c == nil {
			continue
		}
		if merged == nil {
			merged = &GPUTelemetryStats{}
		}
		contributed := fill(&merged.TempMin, c.TempMin)
		contributed = fill(&merged.TempMax, c.TempMax) || contributed
		contributed = fill(&merged.TempMean, c.TempMean) || contributed
		contributed = fill(&merged.UtilMin, c.UtilMin) || contributed
		contributed = fill(&merged.UtilMax, c.UtilMax) || contributed
		contributed = fill(&merged.UtilMean, c.UtilMean) || contributed
		contributed = fill(&merged.ClockMin, c.ClockMin) || contributed
		contributed = fill(&merged.ClockMax, c.ClockMax) || contributed
		contributed = fill(&merged.ClockMean, c.ClockMean) || contributed
		contributed = fill(&merged.MemPeakMiB, c.MemPeakMiB) || contributed
		contributed = fill(&merged.MemTotalMiB, c.MemTotalMiB) || contributed
		contributed = fill(&merged.Throttled, c.Throttled) || contributed
		merged.SampleCount = max(merged.SampleCount, c.SampleCount)
		if contributed {
			sources = append(sources, c.Source)
		}
	}
	if merged == nil {
		return nil
	}
	merged.Source = strings.Join(sources, "+")
	return merged
}

// fill takes src only where the merged value is still absent, and reports
// whether it did, so the caller can record which sources actually contributed.
func fill[T any](dst **T, src *T) bool {
	if *dst != nil || src == nil {
		return false
	}
	*dst = src
	return true
}
