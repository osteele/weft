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
	if stats.TempMin != 60 || stats.TempMax != 70 {
		t.Errorf("Temp min/max = %d/%d, want 60/70", stats.TempMin, stats.TempMax)
	}
	if stats.TempMean != 65 {
		t.Errorf("TempMean = %.1f, want 65.0", stats.TempMean)
	}
	if stats.UtilMin != 80 || stats.UtilMax != 90 {
		t.Errorf("Util min/max = %d/%d, want 80/90", stats.UtilMin, stats.UtilMax)
	}
	if stats.UtilMean != 85 {
		t.Errorf("UtilMean = %.1f, want 85.0", stats.UtilMean)
	}
	if stats.ClockMin != 1500 || stats.ClockMax != 1600 {
		t.Errorf("Clock min/max = %d/%d, want 1500/1600", stats.ClockMin, stats.ClockMax)
	}
	if stats.MemPeakMiB != 8000 {
		t.Errorf("MemPeakMiB = %d, want 8000", stats.MemPeakMiB)
	}
	if stats.MemTotalMiB != 24000 {
		t.Errorf("MemTotalMiB = %d, want 24000", stats.MemTotalMiB)
	}
	if stats.Throttled {
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
	if !stats.Throttled {
		t.Error("Throttled should be true when temp > 80°C")
	}
	if stats.TempMax != 85 {
		t.Errorf("TempMax = %d, want 85", stats.TempMax)
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
	if stats.TempMin != 50 || stats.TempMax != 50 {
		t.Errorf("Temp min/max = %d/%d, want 50/50", stats.TempMin, stats.TempMax)
	}
	if stats.UtilMin != 80 || stats.UtilMax != 80 {
		t.Errorf("Util min/max = %d/%d, want 80/80", stats.UtilMin, stats.UtilMax)
	}
}
