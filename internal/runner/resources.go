package runner

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// ProcCPUHostPct calculates the CPU usage of a process tree as a percentage of total host CPU.
// This sums CPU across the entire process tree (not just the direct PID) because
// the wrapper bash process uses ~0% CPU while the actual work happens in grandchildren.
func ProcCPUHostPct(pid int, cpuCount int) int {
	if cpuCount <= 0 {
		cpuCount = 1
	}

	pids := GetProcessTree(pid)
	totalCPU := 0.0

	for _, p := range pids {
		out, err := exec.Command("ps", "-p", strconv.Itoa(p), "-o", "%cpu=").Output()
		if err != nil {
			continue
		}
		cpu, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		if err != nil {
			continue
		}
		totalCPU += cpu
	}

	// Convert from per-CPU percentage to host percentage
	return int(totalCPU / float64(cpuCount))
}

// ProcResourceUsage collects cumulative resource usage from /proc.
// Returns (userTicks, sysTicks, peakRSSKB).
func ProcResourceUsage(pid int) (userTicks, sysTicks, peakRSSKB int64) {
	pids := GetProcessTree(pid)

	for _, p := range pids {
		// CPU times from /proc/PID/stat (fields 14=utime, 15=stime)
		statPath := fmt.Sprintf("/proc/%d/stat", p)
		data, err := os.ReadFile(statPath)
		if err != nil {
			continue
		}
		fields := strings.Fields(string(data))
		if len(fields) >= 15 {
			ut, _ := strconv.ParseInt(fields[13], 10, 64)
			st, _ := strconv.ParseInt(fields[14], 10, 64)
			userTicks += ut
			sysTicks += st
		}

		// Peak RSS from /proc/PID/status VmHWM
		statusPath := fmt.Sprintf("/proc/%d/status", p)
		statusData, err := os.ReadFile(statusPath)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(statusData), "\n") {
			if strings.HasPrefix(line, "VmHWM:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					hwm, _ := strconv.ParseInt(fields[1], 10, 64)
					if hwm > peakRSSKB {
						peakRSSKB = hwm
					}
				}
			}
		}
	}

	return
}

// TicksToSeconds converts clock ticks to seconds (100 ticks/sec on Linux).
func TicksToSeconds(ticks int64) string {
	return fmt.Sprintf("%.2f", float64(ticks)/100.0)
}

// ProcCurrentRSSKB returns the current RSS (not peak) of a process tree in KB.
// Reads VmRSS from /proc/{pid}/status and sums across the tree.
// Returns 0 on non-Linux or if /proc is unavailable.
func ProcCurrentRSSKB(pid int) int64 {
	if _, err := os.Stat("/proc"); err != nil {
		return 0
	}
	pids := GetProcessTree(pid)
	var totalRSS int64
	for _, p := range pids {
		statusPath := fmt.Sprintf("/proc/%d/status", p)
		data, err := os.ReadFile(statusPath)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					rss, _ := strconv.ParseInt(fields[1], 10, 64)
					totalRSS += rss
				}
			}
		}
	}
	return totalRSS
}

// ProcIOBytes returns cumulative read_bytes and write_bytes for a process tree.
// Returns zeros when /proc is unavailable or the kernel does not expose io stats.
func ProcIOBytes(pid int) (readBytes, writeBytes uint64) {
	if _, err := os.Stat("/proc"); err != nil {
		return 0, 0
	}
	for _, p := range GetProcessTree(pid) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", p))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "read_bytes:":
				value, _ := strconv.ParseUint(fields[1], 10, 64)
				readBytes += value
			case "write_bytes:":
				value, _ := strconv.ParseUint(fields[1], 10, 64)
				writeBytes += value
			}
		}
	}
	return readBytes, writeBytes
}

// HostMemoryKB returns (totalKB, usedKB) for system memory.
// On Linux reads /proc/meminfo; on macOS uses sysctl + vm_stat.
// Returns (0, 0) if unable to determine.
func HostMemoryKB() (totalKB, usedKB int64) {
	totalKB, usedKB, _ = HostMemoryStatsKB()
	return totalKB, usedKB
}

// HostMemoryStatsKB returns (totalKB, usedKB, availableKB) for system memory.
// availableKB is populated from /proc/meminfo MemAvailable on Linux.
func HostMemoryStatsKB() (totalKB, usedKB, availableKB int64) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, 0, 0
		}
		var memTotal, memAvailable int64
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "MemTotal:":
				memTotal, _ = strconv.ParseInt(fields[1], 10, 64)
			case "MemAvailable:":
				memAvailable, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}
		if memTotal > 0 {
			return memTotal, memTotal - memAvailable, memAvailable
		}
		return 0, 0, 0
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0, 0, 0
		}
		memBytes, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		totalKB = memBytes / 1024

		// vm_stat reports pages; page size is typically 16384 on Apple Silicon, 4096 on Intel
		vmOut, err := exec.Command("vm_stat").Output()
		if err != nil {
			return totalKB, 0, 0
		}
		var pageSize int64 = 16384
		var pagesActive, pagesWired, pagesCompressed int64
		for _, line := range strings.Split(string(vmOut), "\n") {
			if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
				// "Mach Virtual Memory Statistics: (page size of 16384 bytes)"
				if idx := strings.Index(line, "page size of "); idx >= 0 {
					rest := line[idx+len("page size of "):]
					if spIdx := strings.Index(rest, " "); spIdx > 0 {
						pageSize, _ = strconv.ParseInt(rest[:spIdx], 10, 64)
					}
				}
			}
			fields := strings.SplitN(line, ":", 2)
			if len(fields) != 2 {
				continue
			}
			val := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(fields[1]), "."))
			n, _ := strconv.ParseInt(val, 10, 64)
			switch strings.TrimSpace(fields[0]) {
			case "Pages active":
				pagesActive = n
			case "Pages wired down":
				pagesWired = n
			case "Pages occupied by compressor":
				pagesCompressed = n
			}
		}
		usedKB = (pagesActive + pagesWired + pagesCompressed) * pageSize / 1024
		return totalKB, usedKB, 0
	}
	return 0, 0, 0
}

// HostLoadAvg1 returns the system 1-minute load average (the same value
// surfaced by `uptime`). Returns 0 if it cannot be determined. Used by the
// agent heartbeat so the dashboard can render a CPU bar for cloud rentals.
func HostLoadAvg1() float64 {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return 0
		}
		fields := strings.Fields(string(data))
		if len(fields) == 0 {
			return 0
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return 0
		}
		return v
	}
	if runtime.GOOS == "darwin" {
		// sysctl -n vm.loadavg → "{ 1.23 4.56 7.89 }"
		out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return 0
		}
		s := strings.ReplaceAll(strings.ReplaceAll(string(out), "{", ""), "}", "")
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return 0
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return 0
		}
		return v
	}
	return 0
}

// MemPressureLevel represents system memory pressure.
type MemPressureLevel string

const (
	MemPressureNormal   MemPressureLevel = "normal"
	MemPressureWarn     MemPressureLevel = "warn"
	MemPressureCritical MemPressureLevel = "critical"
)

// MemoryPressureFromUsage returns the memory pressure level from pre-computed host memory values.
// >90% used → critical, >75% used → warn, else normal.
// Returns "normal" if totalKB is 0.
func MemoryPressureFromUsage(totalKB, usedKB int64) MemPressureLevel {
	if totalKB == 0 {
		return MemPressureNormal
	}
	ratio := float64(usedKB) / float64(totalKB)
	if ratio > 0.90 {
		return MemPressureCritical
	}
	if ratio > 0.75 {
		return MemPressureWarn
	}
	return MemPressureNormal
}

// MemPressureSeverity returns a numeric severity for comparing pressure levels.
// critical=2, warn=1, normal=0.
func MemPressureSeverity(level MemPressureLevel) int {
	switch level {
	case MemPressureCritical:
		return 2
	case MemPressureWarn:
		return 1
	default:
		return 0
	}
}

// HostGPUStats holds system-wide GPU metrics aggregated across all GPUs.
type HostGPUStats struct {
	UtilPct     int // max across GPUs
	MemUsedMiB  int // summed across GPUs
	MemTotalMiB int // summed across GPUs
	TempC       int // max across GPUs
	ClockMHz    int // max across GPUs (graphics clock)
}

// HostGPUMetrics returns system-wide GPU metrics by parsing nvidia-smi.
// Returns a zero HostGPUStats if nvidia-smi is not available.
func HostGPUMetrics() HostGPUStats {
	rows := queryNvidiaSmi("--query-gpu=utilization.gpu,memory.used,memory.total,temperature.gpu,clocks.current.graphics", 5)
	if rows == nil {
		return HostGPUStats{}
	}
	var stats HostGPUStats
	for _, parts := range rows {
		util, _ := strconv.Atoi(parts[0])
		memUsed, _ := strconv.Atoi(parts[1])
		memTotal, _ := strconv.Atoi(parts[2])
		temp, _ := strconv.Atoi(parts[3])
		clock, _ := strconv.Atoi(parts[4])
		if util > stats.UtilPct {
			stats.UtilPct = util
		}
		stats.MemUsedMiB += memUsed
		stats.MemTotalMiB += memTotal
		if temp > stats.TempC {
			stats.TempC = temp
		}
		if clock > stats.ClockMHz {
			stats.ClockMHz = clock
		}
	}
	return stats
}

// ProcGPUMemMiB returns the total GPU memory used by a process tree in MiB.
func ProcGPUMemMiB(pid int) int {
	pids := GetProcessTree(pid)
	return QueryGPUProcessMemory(pids)
}
