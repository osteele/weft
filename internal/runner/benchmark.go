package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// BenchmarkConfig holds thresholds for system idle detection.
type BenchmarkConfig struct {
	CPUThreshold  int // Max CPU % for idle
	RAMThreshold  int // Max RAM % for idle
	GPUThreshold  int // Max GPU utilization % for idle
	VRAMThreshold int // Max VRAM usage % for idle
	IdleSamples   int // Consecutive idle checks required
	CheckInterval int // Seconds between checks
}

// DefaultBenchmarkConfig returns the default benchmark configuration.
func DefaultBenchmarkConfig() BenchmarkConfig {
	return BenchmarkConfig{
		CPUThreshold:  intFromEnv("WEFT_BENCHMARK_CPU", 5),
		RAMThreshold:  intFromEnv("WEFT_BENCHMARK_RAM", 20),
		GPUThreshold:  intFromEnv("WEFT_BENCHMARK_GPU", 5),
		VRAMThreshold: intFromEnv("WEFT_BENCHMARK_VRAM", 5),
		IdleSamples:   intFromEnv("WEFT_BENCHMARK_SAMPLES", 3),
		CheckInterval: intFromEnv("WEFT_BENCHMARK_INTERVAL", 10),
	}
}

func intFromEnv(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

// SystemIdleCheck checks if the system is idle enough for benchmark jobs.
// Returns "" if idle, or a reason string if not.
func (cfg BenchmarkConfig) SystemIdleCheck() string {
	var reasons []string

	cpu := hostCPUInstantPct()
	if cpu > cfg.CPUThreshold {
		reasons = append(reasons, fmt.Sprintf("cpu=%d%%>%d%%", cpu, cfg.CPUThreshold))
	}

	ram := hostRAMUsagePct()
	if ram > cfg.RAMThreshold {
		reasons = append(reasons, fmt.Sprintf("ram=%d%%>%d%%", ram, cfg.RAMThreshold))
	}

	if hasNvidiaSMI() {
		gpu := hostGPUUtilizationPct()
		if gpu > cfg.GPUThreshold {
			reasons = append(reasons, fmt.Sprintf("gpu=%d%%>%d%%", gpu, cfg.GPUThreshold))
		}
		vram := hostGPUVRAMPct()
		if vram > cfg.VRAMThreshold {
			reasons = append(reasons, fmt.Sprintf("vram=%d%%>%d%%", vram, cfg.VRAMThreshold))
		}
	}

	return strings.Join(reasons, ", ")
}

// HasExclusiveOrBenchmarkTag checks if a job has either the "exclusive" or "benchmark" tag.
func HasExclusiveOrBenchmarkTag(job *RunnerJob) bool {
	return HasTag(job.Data, "exclusive") || HasTag(job.Data, "benchmark")
}

// HasBenchmarkTag checks if a job has the "benchmark" tag.
func HasBenchmarkTag(job *RunnerJob) bool {
	return HasTag(job.Data, "benchmark")
}

// AnyRunningExclusive checks if any running job has exclusive or benchmark tags.
func AnyRunningExclusive(state *State, queueDir string) bool {
	for _, id := range state.RunningIDs() {
		job, err := ReadJobFile(queueDir, mustParseInt64(id))
		if err != nil {
			continue
		}
		rj := &RunnerJob{Data: job, ID: mustParseInt64(id)}
		if HasExclusiveOrBenchmarkTag(rj) {
			return true
		}
	}
	return false
}

func mustParseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func hasNvidiaSMI() bool {
	_, err := exec.LookPath("nvidia-smi")
	return err == nil
}

// hostCPUInstantPct returns instantaneous CPU usage as a percentage.
// On Linux, reads /proc/stat delta. On macOS, uses load average.
func hostCPUInstantPct() int {
	// Try /proc/stat (Linux)
	data1, err := os.ReadFile("/proc/stat")
	if err == nil {
		// Read, sleep 1s, read again for delta
		// For simplicity in Go, use a single sample with the vmstat approach
		out, err := exec.Command("bash", "-c",
			`awk '/^cpu / {u1=$2;n1=$3;s1=$4;i1=$5;w1=$6;q1=$7;sq1=$8;st1=$9}END{}' /proc/stat; `+
				`sleep 1; `+
				`awk '/^cpu / {u2=$2;n2=$3;s2=$4;i2=$5;w2=$6;q2=$7;sq2=$8;st2=$9; `+
				`t1=u1+n1+s1+i1+w1+q1+sq1+st1; t2=u2+n2+s2+i2+w2+q2+sq2+st2; `+
				`printf "%.0f", ((t2-t1)-(i2+w2-i1-w1))*100/(t2-t1)}' /proc/stat`).Output()
		if err == nil {
			n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
			return n
		}
		_ = data1 // suppress unused
	}

	// macOS fallback: load average / CPU count
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0
	}
	load, _ := strconv.ParseFloat(fields[1], 64)
	cpuCount := DetectCPUCount()
	pct := int(load * 100.0 / float64(cpuCount))
	if pct > 100 {
		pct = 100
	}
	return pct
}

// hostRAMUsagePct returns RAM usage as a percentage.
func hostRAMUsagePct() int {
	// Try `free` (Linux)
	out, err := exec.Command("free", "-b").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "Mem:") {
				fields := strings.Fields(line)
				if len(fields) >= 7 {
					total, _ := strconv.ParseFloat(fields[1], 64)
					available, _ := strconv.ParseFloat(fields[6], 64)
					if total > 0 {
						return int((total - available) * 100 / total)
					}
				}
			}
		}
	}

	// macOS fallback
	memOut, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	totalBytes, _ := strconv.ParseFloat(strings.TrimSpace(string(memOut)), 64)
	vmOut, err := exec.Command("vm_stat").Output()
	if err != nil || totalBytes <= 0 {
		return 0
	}
	var freePages float64
	for _, line := range strings.Split(string(vmOut), "\n") {
		if strings.Contains(line, "Pages free") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				freePages, _ = strconv.ParseFloat(strings.TrimSuffix(fields[2], "."), 64)
			}
		}
	}
	freeBytes := freePages * 4096
	return int((totalBytes - freeBytes) * 100 / totalBytes)
}

// hostGPUUtilizationPct returns max GPU utilization across all GPUs.
func hostGPUUtilizationPct() int {
	rows := queryNvidiaSmi("--query-gpu=utilization.gpu", 1)
	if rows == nil {
		return 0
	}
	maxUtil := 0
	for _, parts := range rows {
		n, _ := strconv.Atoi(parts[0])
		if n > maxUtil {
			maxUtil = n
		}
	}
	return maxUtil
}

// hostGPUVRAMPct returns max VRAM usage percentage across all GPUs.
func hostGPUVRAMPct() int {
	rows := queryNvidiaSmi("--query-gpu=memory.used,memory.total", 2)
	if rows == nil {
		return 0
	}
	maxPct := 0
	for _, parts := range rows {
		used, _ := strconv.ParseFloat(parts[0], 64)
		total, _ := strconv.ParseFloat(parts[1], 64)
		if total > 0 {
			pct := int(used * 100 / total)
			if pct > maxPct {
				maxPct = pct
			}
		}
	}
	return maxPct
}
