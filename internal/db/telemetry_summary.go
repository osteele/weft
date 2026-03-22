package db

import "math"

// ThermalThrottleThresholdC is the GPU temperature (°C) above which
// thermal throttling is likely.
const ThermalThrottleThresholdC = 80

// GPUTelemetryStats summarises GPU telemetry from timeseries samples.
type GPUTelemetryStats struct {
	SampleCount int     `json:"sample_count"`
	TempMin     int     `json:"temp_min_c"`
	TempMax     int     `json:"temp_max_c"`
	TempMean    float64 `json:"temp_mean_c"`
	UtilMin     int     `json:"util_min_pct"`
	UtilMax     int     `json:"util_max_pct"`
	UtilMean    float64 `json:"util_mean_pct"`
	ClockMin    int     `json:"clock_min_mhz"`
	ClockMax    int     `json:"clock_max_mhz"`
	ClockMean   float64 `json:"clock_mean_mhz"`
	MemPeakMiB  int     `json:"mem_peak_mib"`
	MemTotalMiB int     `json:"mem_total_mib"`
	Throttled   bool    `json:"throttled"`
}

// ComputeGPUTelemetryStats computes aggregate GPU statistics from timeseries
// samples. Returns nil if no samples contain GPU data (temp, util, or clock > 0).
func ComputeGPUTelemetryStats(samples []TimeseriesSample) *GPUTelemetryStats {
	var stats GPUTelemetryStats
	tempMin, utilMin, clockMin := math.MaxInt, math.MaxInt, math.MaxInt
	var tempSum, utilSum, clockSum float64
	var tempCount, utilCount, clockCount int

	for _, s := range samples {
		if s.GPUTempC > 0 {
			tempMin = min(tempMin, s.GPUTempC)
			stats.TempMax = max(stats.TempMax, s.GPUTempC)
			tempSum += float64(s.GPUTempC)
			tempCount++
			if s.GPUTempC > ThermalThrottleThresholdC {
				stats.Throttled = true
			}
		}
		if s.GPUUtilPct > 0 {
			utilMin = min(utilMin, s.GPUUtilPct)
			stats.UtilMax = max(stats.UtilMax, s.GPUUtilPct)
			utilSum += float64(s.GPUUtilPct)
			utilCount++
		}
		if s.GPUClockMHz > 0 {
			clockMin = min(clockMin, s.GPUClockMHz)
			stats.ClockMax = max(stats.ClockMax, s.GPUClockMHz)
			clockSum += float64(s.GPUClockMHz)
			clockCount++
		}
		stats.MemPeakMiB = max(stats.MemPeakMiB, s.GPUMemUsedMiB)
		stats.MemTotalMiB = max(stats.MemTotalMiB, s.GPUMemTotalMiB)
	}

	if tempCount+utilCount+clockCount == 0 {
		return nil
	}

	stats.SampleCount = max(tempCount, max(utilCount, clockCount))
	if tempCount > 0 {
		stats.TempMin = tempMin
		stats.TempMean = math.Round(tempSum/float64(tempCount)*10) / 10
	}
	if utilCount > 0 {
		stats.UtilMin = utilMin
		stats.UtilMean = math.Round(utilSum/float64(utilCount)*10) / 10
	}
	if clockCount > 0 {
		stats.ClockMin = clockMin
		stats.ClockMean = math.Round(clockSum/float64(clockCount)*10) / 10
	}
	return &stats
}
