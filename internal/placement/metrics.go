package placement

import (
	"database/sql"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queuerunner"
)

// CollectMetrics probes hosts in parallel via a single SSH round-trip per host,
// collecting both liveness and live utilization metrics. A host that fails the
// SSH call is omitted from the returned map (treated as unreachable).
func CollectMetrics(db *sql.DB, hosts []string, timeout time.Duration) map[string]*HostMetrics {
	result := make(map[string]*HostMetrics, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			status, err := ops.FetchHostStatusCombined(db, h, queuerunner.StatusCommand(), timeout)
			if err != nil {
				return // host unreachable
			}

			var queueStatus *queuerunner.StatusInfo
			if status.ExtraOutput != "" {
				queueStatus = queuerunner.ParseStatus(status.ExtraOutput)
			}

			m := HostMetricsFromHostInfo(status.Host, queueStatus)

			mu.Lock()
			result[h] = m
			mu.Unlock()
		}(host)
	}

	wg.Wait()
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
