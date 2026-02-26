package runner

import "testing"

func TestDefaultAllotment(t *testing.T) {
	cfg := DefaultCPUConfig()

	tests := []struct {
		cpuCount int
		want     int
	}{
		{1, 100}, // 7 cores / 1 CPU = 700% capped to 100%
		{8, 87},  // 7/8 = 87%
		{16, 43}, // 7/16 = 43%
		{32, 21}, // 7/32 = 21%
		{128, 5}, // 7/128 = 5%
		{0, 60},  // Invalid CPU count defaults to 60%
	}

	for _, tt := range tests {
		got := cfg.DefaultAllotment(tt.cpuCount)
		if got != tt.want {
			t.Errorf("DefaultAllotment(%d) = %d, want %d", tt.cpuCount, got, tt.want)
		}
	}
}

func TestAdjustAllotment_IncreasesWhenOver(t *testing.T) {
	cfg := DefaultCPUConfig()

	// Samples all above allotment
	samples := make([]int, cfg.SampleCount())
	for i := range samples {
		samples[i] = 50
	}
	overHist := []int{1, 1, 1}

	newAllotment, _, _ := cfg.AdjustAllotment(30, samples, overHist, nil, 30)
	if newAllotment <= 30 {
		t.Errorf("expected increase from 30, got %d", newAllotment)
	}
}

func TestAdjustAllotment_DecreasesWhenUnder(t *testing.T) {
	cfg := DefaultCPUConfig()

	samples := make([]int, cfg.SampleCount())
	for i := range samples {
		samples[i] = 10
	}
	underHist := []int{1, 1, 1}

	newAllotment, _, _ := cfg.AdjustAllotment(50, samples, nil, underHist, 50)
	if newAllotment >= 50 {
		t.Errorf("expected decrease from 50, got %d", newAllotment)
	}
}

func TestAdjustAllotment_NoChangeWithFewSamples(t *testing.T) {
	cfg := DefaultCPUConfig()

	// Too few samples
	samples := []int{50, 50}
	newAllotment, _, _ := cfg.AdjustAllotment(30, samples, nil, nil, 30)
	if newAllotment != 30 {
		t.Errorf("expected no change, got %d", newAllotment)
	}
}

func TestAdjustAllotment_RespectsMinAllotment(t *testing.T) {
	cfg := DefaultCPUConfig()

	samples := make([]int, cfg.SampleCount())
	underHist := []int{1, 1, 1}

	newAllotment, _, _ := cfg.AdjustAllotment(cfg.MinAllotment, samples, nil, underHist, cfg.MinAllotment)
	if newAllotment < cfg.MinAllotment {
		t.Errorf("expected minimum %d, got %d", cfg.MinAllotment, newAllotment)
	}
}
