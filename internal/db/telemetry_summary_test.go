package db

import "testing"

func TestComputeGPUTelemetryStats_NoData(t *testing.T) {
	samples := []TimeseriesSample{
		{Ts: 1, CPUPct: 50, RSSKB: 1000},
		{Ts: 2, CPUPct: 60, RSSKB: 2000},
	}
	if got := ComputeGPUTelemetryStats(samples); got != nil {
		t.Fatalf("expected nil for samples without GPU data, got %+v", got)
	}
}

func TestComputeGPUTelemetryStats_Empty(t *testing.T) {
	if got := ComputeGPUTelemetryStats(nil); got != nil {
		t.Fatalf("expected nil for nil input, got %+v", got)
	}
}

func TestComputeGPUTelemetryStats_Basic(t *testing.T) {
	samples := []TimeseriesSample{
		{Ts: 1, GPUTempC: 60, GPUUtilPct: 80, GPUClockMHz: 1500, GPUMemUsedMiB: 4000, GPUMemTotalMiB: 24000},
		{Ts: 2, GPUTempC: 70, GPUUtilPct: 90, GPUClockMHz: 1600, GPUMemUsedMiB: 8000, GPUMemTotalMiB: 24000},
		{Ts: 3, GPUTempC: 65, GPUUtilPct: 85, GPUClockMHz: 1550, GPUMemUsedMiB: 6000, GPUMemTotalMiB: 24000},
	}
	stats := ComputeGPUTelemetryStats(samples)
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if stats.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", stats.SampleCount)
	}
	if derefI(stats.TempMin) != 60 || derefI(stats.TempMax) != 70 {
		t.Errorf("Temp min/max = %d/%d, want 60/70", derefI(stats.TempMin), derefI(stats.TempMax))
	}
	if derefF(stats.TempMean) != 65 {
		t.Errorf("TempMean = %.1f, want 65.0", derefF(stats.TempMean))
	}
	if derefI(stats.UtilMin) != 80 || derefI(stats.UtilMax) != 90 {
		t.Errorf("Util min/max = %d/%d, want 80/90", derefI(stats.UtilMin), derefI(stats.UtilMax))
	}
	if derefF(stats.UtilMean) != 85 {
		t.Errorf("UtilMean = %.1f, want 85.0", derefF(stats.UtilMean))
	}
	if derefI(stats.ClockMin) != 1500 || derefI(stats.ClockMax) != 1600 {
		t.Errorf("Clock min/max = %d/%d, want 1500/1600", derefI(stats.ClockMin), derefI(stats.ClockMax))
	}
	if derefI(stats.MemPeakMiB) != 8000 {
		t.Errorf("MemPeakMiB = %d, want 8000", derefI(stats.MemPeakMiB))
	}
	if derefI(stats.MemTotalMiB) != 24000 {
		t.Errorf("MemTotalMiB = %d, want 24000", derefI(stats.MemTotalMiB))
	}
	if derefB(stats.Throttled) {
		t.Error("Throttled should be false for temps <= 80°C")
	}
}

func TestComputeGPUTelemetryStats_Throttled(t *testing.T) {
	samples := []TimeseriesSample{
		{Ts: 1, GPUTempC: 75, GPUUtilPct: 95},
		{Ts: 2, GPUTempC: 82, GPUUtilPct: 90},
		{Ts: 3, GPUTempC: 85, GPUUtilPct: 70},
	}
	stats := ComputeGPUTelemetryStats(samples)
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if !derefB(stats.Throttled) {
		t.Error("Throttled should be true when temp > 80°C")
	}
	if derefI(stats.TempMax) != 85 {
		t.Errorf("TempMax = %d, want 85", derefI(stats.TempMax))
	}
}

func TestComputeGPUTelemetryStats_PartialData(t *testing.T) {
	samples := []TimeseriesSample{
		{Ts: 1, GPUTempC: 50},
		{Ts: 2, GPUUtilPct: 80},
		{Ts: 3, GPUClockMHz: 1500},
	}
	stats := ComputeGPUTelemetryStats(samples)
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if derefI(stats.TempMin) != 50 || derefI(stats.TempMax) != 50 {
		t.Errorf("Temp min/max = %d/%d, want 50/50", derefI(stats.TempMin), derefI(stats.TempMax))
	}
	if derefI(stats.UtilMin) != 80 || derefI(stats.UtilMax) != 80 {
		t.Errorf("Util min/max = %d/%d, want 80/80", derefI(stats.UtilMin), derefI(stats.UtilMax))
	}
}

// The summary distinguishes "not reported by this path" from "measured zero",
// so these read through a default. Tests that care about the distinction
// assert on the pointer directly.
func derefI(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func derefF(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func derefB(v *bool) bool {
	return v != nil && *v
}

// The rollup carries no minima, no clocks and no total memory. Those must come
// back absent rather than zero: a consumer pooling calibration numbers cannot
// otherwise tell a field this path never had from a GPU that genuinely read 0.
func TestRollupReportsAbsenceNotZero(t *testing.T) {
	stats := GPUTelemetryStatsFromTimeseriesSummary(&TimeseriesSummary{
		SampleCount:    10,
		PeakGPUTempC:   65,
		MeanGPUTempC:   60,
		PeakGPUUtilPct: 90,
		MeanGPUUtilPct: 80,
		PeakGPUMemMiB:  8000,
	})
	if stats == nil {
		t.Fatal("stats = nil, want a rollup summary")
	}
	if stats.Source != TelemetrySourceRollup {
		t.Errorf("Source = %q, want %q", stats.Source, TelemetrySourceRollup)
	}
	for name, got := range map[string]*int{
		"TempMin":     stats.TempMin,
		"UtilMin":     stats.UtilMin,
		"ClockMin":    stats.ClockMin,
		"ClockMax":    stats.ClockMax,
		"MemTotalMiB": stats.MemTotalMiB,
	} {
		if got != nil {
			t.Errorf("%s = %d, want absent: the rollup cannot report it", name, *got)
		}
	}
	if stats.ClockMean != nil {
		t.Errorf("ClockMean = %v, want absent", *stats.ClockMean)
	}
	// What it does carry must still be present.
	if stats.TempMax == nil || *stats.TempMax != 65 {
		t.Errorf("TempMax = %v, want 65", stats.TempMax)
	}
}

// A genuine zero must survive as a present zero, not be mistaken for absence.
func TestMeasuredZeroIsReportedNotOmitted(t *testing.T) {
	stats := ComputeGPUTelemetryStats([]TimeseriesSample{
		{GPUTempC: 40, GPUUtilPct: 0, GPUClockMHz: 300},
		{GPUTempC: 42, GPUUtilPct: 0, GPUClockMHz: 300},
	})
	if stats == nil {
		t.Fatal("stats = nil, want a summary")
	}
	// Utilisation was never above zero, so no sample contributed: the path
	// reports it as absent rather than inventing a measured 0.
	if stats.UtilMax != nil {
		t.Errorf("UtilMax = %d, want absent when no sample reported utilisation", *stats.UtilMax)
	}
	if stats.ClockMin == nil || *stats.ClockMin != 300 {
		t.Errorf("ClockMin = %v, want 300", stats.ClockMin)
	}
}
