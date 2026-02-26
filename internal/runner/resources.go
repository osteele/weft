package runner

import (
	"fmt"
	"os"
	"os/exec"
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

// ProcGPUMemMiB returns the total GPU memory used by a process tree in MiB.
func ProcGPUMemMiB(pid int) int {
	pids := GetProcessTree(pid)
	return QueryGPUProcessMemory(pids)
}
