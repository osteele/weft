package runner

import (
	"os"
	"time"
)

// SampleJob collects one telemetry sample for a running job: CPU, memory,
// GPU, and host-wide metrics. It updates high-water marks on rs, writes a
// timeseries sample and heartbeat, and returns the collected memory pressure
// and host CPU percentage.
//
// Both the single-job runner and the queue runner call this function.
func SampleJob(pid, pgid int, cpuCount int, paths JobPaths, rs *RunningJobState, tenant string) (MemPressureLevel, int) {
	now := time.Now()

	hostPct := ProcCPUHostPct(pid, cpuCount)
	WriteSample(paths, now.Unix(), hostPct, nil)

	// Resource usage from /proc
	rusagePID := pid
	if pgid > 0 {
		rusagePID = pgid
	}
	if _, err := os.Stat("/proc"); err == nil {
		userTicks, sysTicks, peakRSS := ProcResourceUsage(rusagePID)
		rs.RusageUserCPU = TicksToSeconds(userTicks)
		rs.RusageSysCPU = TicksToSeconds(sysTicks)
		if peakRSS > rs.RusagePeakRSS {
			rs.RusagePeakRSS = peakRSS
		}
	}

	// GPU memory sampling
	gpuMem := ProcGPUMemMiB(pid)
	if gpuMem > rs.RusageMaxGPU {
		rs.RusageMaxGPU = gpuMem
	}

	// Timeseries sample
	currentRSS := ProcCurrentRSSKB(rusagePID)
	hostTotal, hostUsed := HostMemoryKB()
	diskUsed, diskTotal := ProbeDiskUsage()
	gpuStats := HostGPUMetrics()
	pressure := MemoryPressureFromUsage(hostTotal, hostUsed)

	sample := TimeseriesSample{
		Ts:             now.Unix(),
		CPUPct:         hostPct,
		RSSKB:          currentRSS,
		GPUMiB:         gpuMem,
		DiskFreeBytes:  diskTotal - diskUsed,
		DiskTotalBytes: diskTotal,
		HostRSSKB:      hostUsed,
		HostMemTotal:   hostTotal,
		GPUUtilPct:     gpuStats.UtilPct,
		GPUMemUsed:     gpuStats.MemUsedMiB,
		GPUMemTotal:    gpuStats.MemTotalMiB,
		GPUTempC:       gpuStats.TempC,
		GPUClockMHz:    gpuStats.ClockMHz,
		MemPressure:    string(pressure),
		Tenant:         tenant,
	}
	WriteTimeseriesSample(paths, sample)

	// High-water marks
	if MemPressureSeverity(pressure) > MemPressureSeverity(rs.PeakMemPressure) {
		rs.PeakMemPressure = pressure
	}
	if hostTotal > 0 {
		ratio := float64(hostUsed) / float64(hostTotal)
		if ratio > rs.PeakHostMemRatio {
			rs.PeakHostMemRatio = ratio
		}
	}
	if currentRSS > rs.PeakRSSFromTS {
		rs.PeakRSSFromTS = currentRSS
	}

	rs.LastHeartbeat = now.Unix()
	rs.LastSample = now.Unix()
	WriteHeartbeat(paths, now.Unix())

	return pressure, hostPct
}
