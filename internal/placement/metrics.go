package placement

import (
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queuerunner"
)

var collectMetricsFetchHostStatus = ops.TryFetchHostStatusCombined

// CollectMetrics probes hosts in parallel via a single SSH round-trip per host,
// collecting both liveness and live utilization metrics. A host that fails the
// SSH call is omitted from the returned map (treated as unreachable). Collection
// is best-effort: callers such as autopilot use it on hot paths and must not
// wait for a wedged SSH session or provider-side shell startup.
func CollectMetrics(db *sql.DB, hosts []string, timeout time.Duration) map[string]*HostMetrics {
	result := make(map[string]*HostMetrics, len(hosts))
	type hostResult struct {
		host    string
		metrics *HostMetrics
	}
	results := make(chan hostResult, len(hosts))

	for _, host := range hosts {
		go func(h string) {
			status, err := collectMetricsFetchHostStatus(db, h, queuerunner.StatusCommand(), timeout)
			if err != nil {
				results <- hostResult{host: h}
				return // host unreachable
			}

			var queueStatus *queuerunner.StatusInfo
			if status.ExtraOutput != "" {
				queueStatus = queuerunner.ParseStatus(status.ExtraOutput)
			}

			m := HostMetricsFromHostInfo(status.Host, queueStatus)

			// Record contention observation for future estimation
			RecordContentionObs(db, h, m)

			results <- hostResult{host: h, metrics: m}
		}(host)
	}

	var deadline <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		deadline = timer.C
		defer timer.Stop()
	}
	for range hosts {
		if timeout <= 0 {
			r := <-results
			if r.metrics != nil {
				result[r.host] = r.metrics
			}
			continue
		}
		select {
		case r := <-results:
			if r.metrics != nil {
				result[r.host] = r.metrics
			}
		case <-deadline:
			return result
		}
	}
	return result
}

// HostMetricsFromHostInfo converts a hostinfo.Host and optional queue status
// into a HostMetrics suitable for placement scoring.
func HostMetricsFromHostInfo(host *hostinfo.Host, queueStatus *queuerunner.StatusInfo) *HostMetrics {
	m := &HostMetrics{}

	if cpuPct, ok := hostinfo.HostCPULoadPercent(host); ok {
		m.CPUPercent = cpuPct
	}
	if ramPct, ok := hostinfo.HostMemUsagePercent(host); ok {
		m.RAMPercent = ramPct
	}
	if gpuPct, ok := hostinfo.HostGPULoadPercent(host); ok {
		m.GPUPercent = gpuPct
	}

	// Free RAM: total - used (in KB)
	m.FreeRAMKB = computeFreeRAMKB(host.MemTotal, host.MemUsed)

	// Per-device GPU free memory
	deviceFree := make(map[string]int64)
	var totalFreeGPUMiB int64
	for _, gpu := range host.GPUs {
		freeMiB := computeGPUFreeMiB(gpu.MemTotal, gpu.MemUsed)
		if freeMiB >= 0 {
			deviceFree[strconv.Itoa(gpu.Index)] = freeMiB
			totalFreeGPUMiB += freeMiB
		}
	}
	if len(deviceFree) > 0 {
		m.GPUDeviceFreeMemMiB = deviceFree
		m.FreeGPUMemMiB = totalFreeGPUMiB
	}

	// Queue depth
	if queueStatus != nil {
		m.QueueDepth = queueStatus.QueuedJobCount
	}

	return m
}

// computeFreeRAMKB computes free RAM in KB from total and used strings like "128G", "58G".
func computeFreeRAMKB(totalStr, usedStr string) int64 {
	totalGB := parseMemValue(totalStr)
	usedGB := parseMemValue(usedStr)
	if totalGB <= 0 || usedGB < 0 {
		return 0
	}
	freeGB := totalGB - usedGB
	if freeGB < 0 {
		freeGB = 0
	}
	return int64(freeGB * 1024 * 1024) // GB to KB
}

// computeGPUFreeMiB computes free GPU memory in MiB from total and used strings.
// GPU memory strings can be like "80 GiB", "12 MiB", "12345 MiB", etc.
func computeGPUFreeMiB(totalStr, usedStr string) int64 {
	totalMiB := parseSizeToMiB(totalStr)
	usedMiB := parseSizeToMiB(usedStr)
	if totalMiB <= 0 {
		return -1
	}
	free := totalMiB - usedMiB
	if free < 0 {
		free = 0
	}
	return int64(free)
}

// parseMemValue extracts a numeric GB value from strings like "128G", "58G", "256GB".
func parseMemValue(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0
	}
	s = strings.TrimSuffix(s, "GB")
	s = strings.TrimSuffix(s, "gb")
	s = strings.TrimSuffix(s, "G")
	s = strings.TrimSuffix(s, "g")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseSizeToMiB converts a size string like "80 GiB", "12345 MiB", "24576 MiB" to MiB.
func parseSizeToMiB(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0
	}

	// Split into number and unit
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return 0
	}

	numStr := parts[0]
	unit := ""
	if len(parts) > 1 {
		unit = strings.ToLower(parts[1])
	}

	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}

	switch unit {
	case "gib", "gb", "g":
		return num * 1024
	case "mib", "mb", "m", "":
		return num
	case "kib", "kb", "k":
		return num / 1024
	default:
		return num // assume MiB
	}
}
