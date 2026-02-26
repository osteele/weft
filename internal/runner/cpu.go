package runner

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// CPUConfig holds tuning parameters for CPU-based concurrency control.
type CPUConfig struct {
	HostUtilizationTarget int // Maximum total allotment % before blocking new jobs
	DefaultAllotmentCores int // Default cores per job (converted to % of total)
	WarmupDuration        int // Seconds before sampling starts for a new job
	SampleInterval        int // Seconds between CPU samples
	SampleWindow          int // Seconds of sample history to keep
	HysteresisWindow      int // Number of over/under decisions to keep
	HysteresisThreshold   int // How many must agree before adjusting
	IncreaseStep          int // Allotment % increase per adjustment
	DecayStep             int // Allotment % decrease per adjustment
	MinAllotment          int // Minimum allotment %
	MaxAllotment          int // Maximum allotment %
}

// DefaultCPUConfig returns the default CPU configuration matching the bash runner.
func DefaultCPUConfig() CPUConfig {
	return CPUConfig{
		HostUtilizationTarget: 80,
		DefaultAllotmentCores: 7,
		WarmupDuration:        120,
		SampleInterval:        15,
		SampleWindow:          60,
		HysteresisWindow:      5,
		HysteresisThreshold:   3,
		IncreaseStep:          10,
		DecayStep:             10,
		MinAllotment:          10,
		MaxAllotment:          100,
	}
}

// DetectCPUCount returns the number of CPU cores available.
func DetectCPUCount() int {
	// Try nproc first (Linux)
	if out, err := exec.Command("nproc").Output(); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && n > 0 {
			return n
		}
	}
	// Fall back to Go's runtime
	return runtime.NumCPU()
}

// DefaultAllotment calculates the default CPU allotment percentage for a job
// based on the number of cores and the default cores-per-job setting.
func (cfg CPUConfig) DefaultAllotment(cpuCount int) int {
	if cpuCount <= 0 {
		return 60
	}
	pct := (cfg.DefaultAllotmentCores * 100) / cpuCount
	if pct < 1 {
		pct = 1
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// SampleCount returns the number of samples to keep in the sliding window.
func (cfg CPUConfig) SampleCount() int {
	if cfg.SampleInterval <= 0 {
		return 4
	}
	return cfg.SampleWindow / cfg.SampleInterval
}

// AdjustAllotment determines if and how to adjust a job's allotment based on
// CPU sample history and hysteresis.
// Returns the new allotment (may be unchanged).
func (cfg CPUConfig) AdjustAllotment(currentAllotment int, samples []int, overHist, underHist []int, totalAllotment int) (newAllotment int, newOverHist, newUnderHist []int) {
	sampleCount := cfg.SampleCount()
	if len(samples) < sampleCount {
		return currentAllotment, overHist, underHist
	}

	avg := average(samples)

	// Determine if over or under
	over := 0
	if avg > currentAllotment {
		over = 1
	}
	under := 0
	if avg < currentAllotment-10 {
		under = 1
	}

	overHist = appendBounded(overHist, over, cfg.HysteresisWindow)
	underHist = appendBounded(underHist, under, cfg.HysteresisWindow)

	overCount := countOnes(overHist)
	underCount := countOnes(underHist)

	if overCount >= cfg.HysteresisThreshold {
		// Calculate headroom
		headroom := cfg.HostUtilizationTarget - totalAllotment + currentAllotment
		newVal := avg + cfg.IncreaseStep
		if newVal > cfg.MaxAllotment {
			newVal = cfg.MaxAllotment
		}
		if newVal > headroom {
			newVal = headroom
		}
		if newVal < 0 {
			newVal = 0
		}
		if newVal < cfg.MinAllotment {
			newVal = cfg.MinAllotment
		}
		if newVal > currentAllotment {
			return newVal, nil, nil // Reset histories
		}
		return currentAllotment, overHist, underHist
	}

	if underCount >= cfg.HysteresisThreshold {
		newVal := currentAllotment - cfg.DecayStep
		if newVal < cfg.MinAllotment {
			newVal = cfg.MinAllotment
		}
		return newVal, nil, nil // Reset histories
	}

	return currentAllotment, overHist, underHist
}

func average(vals []int) int {
	if len(vals) == 0 {
		return 0
	}
	sum := 0
	for _, v := range vals {
		sum += v
	}
	return sum / len(vals)
}

func appendBounded(hist []int, val int, maxLen int) []int {
	hist = append(hist, val)
	if len(hist) > maxLen {
		hist = hist[len(hist)-maxLen:]
	}
	return hist
}

func countOnes(hist []int) int {
	count := 0
	for _, v := range hist {
		if v == 1 {
			count++
		}
	}
	return count
}
