package hostinfo

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

var humanSizePattern = regexp.MustCompile(`(?i)^([\d.]+)\s*([kmgtp]?i?[b]?)?$`)

// HostCPULoadPercent returns the CPU load percent based on 1-minute load average.
func HostCPULoadPercent(host *Host) (int, bool) {
	if host == nil || host.LoadAvg == "" || host.CPUs <= 0 {
		return host.lastCPUPct, host.lastCPUPct > 0
	}
	loadFields := strings.Fields(strings.ReplaceAll(host.LoadAvg, ",", " "))
	if len(loadFields) == 0 {
		return host.lastCPUPct, host.lastCPUPct > 0
	}
	load, err := strconv.ParseFloat(strings.TrimSpace(loadFields[0]), 64)
	if err != nil {
		return host.lastCPUPct, host.lastCPUPct > 0
	}
	pct := int(math.Round((load / float64(host.CPUs)) * 100))
	if pct < 0 {
		pct = 0
	}
	if pct > 200 {
		pct = 200
	}
	host.lastCPUPct = pct
	return pct, true
}

// HostMemUsagePercent returns memory usage percent based on MemUsed and MemTotal.
func HostMemUsagePercent(host *Host) (int, bool) {
	used, okUsed := parseSizeToGiB(host.MemUsed)
	total, okTotal := parseSizeToGiB(host.MemTotal)
	if !okUsed || !okTotal || total <= 0 {
		return host.lastRAMPct, host.lastRAMPct > 0
	}
	pct := int(math.Round((used / total) * 100))
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	host.lastRAMPct = pct
	return pct, true
}

// HostGPULoadPercent returns the max GPU utilization or memory usage percent.
func HostGPULoadPercent(host *Host) (int, bool) {
	maxLoad := -1
	for _, gpu := range host.GPUs {
		if gpu.Utilization > maxLoad {
			maxLoad = gpu.Utilization
		}

		used, okUsed := parseSizeToGiB(gpu.MemUsed)
		total, okTotal := parseSizeToGiB(gpu.MemTotal)
		if okUsed && okTotal && total > 0 {
			memPct := int(math.Round((used / total) * 100))
			if memPct > maxLoad {
				maxLoad = memPct
			}
		}
	}
	if maxLoad < 0 {
		return host.lastGPUPct, host.lastGPUPct > 0
	}
	if maxLoad > 100 {
		maxLoad = 100
	}
	host.lastGPUPct = maxLoad
	return maxLoad, true
}

func parseSizeToGiB(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "-" {
		return 0, false
	}
	matches := humanSizePattern.FindStringSubmatch(value)
	if matches == nil {
		return 0, false
	}
	num, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, false
	}
	unit := strings.ToLower(matches[2])
	switch unit {
	case "", "b":
		return num / (1024 * 1024 * 1024), true
	case "k", "kb":
		return num / (1024 * 1024), true
	case "m", "mb":
		return num / 1024, true
	case "g", "gb":
		return num, true
	case "t", "tb":
		return num * 1024, true
	case "p", "pb":
		return num * 1024 * 1024, true
	case "ki", "kib":
		return num / (1024 * 1024), true
	case "mi", "mib":
		return num / 1024, true
	case "gi", "gib":
		return num, true
	case "ti", "tib":
		return num * 1024, true
	case "pi", "pib":
		return num * 1024 * 1024, true
	default:
		return 0, false
	}
}
