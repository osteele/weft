package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/runner"
)

// HeartbeatSample is the JSON payload written to the instance heartbeat R2 key.
type HeartbeatSample struct {
	Ts             int64  `json:"ts"`
	Phase          string `json:"phase"`
	GPUUtilPct     int    `json:"gpu_util_pct"`
	GPUMemUsedMiB  int    `json:"gpu_mem_used_mib"`
	GPUMemTotalMiB int    `json:"gpu_mem_total_mib"`
	GPUTempC       int    `json:"gpu_temp_c"`
	HostRSSKB      int64  `json:"host_rss_kb"`
	HostMemTotalKB int64  `json:"host_mem_total_kb"`
	MemAvailableKB int64  `json:"mem_available_kb"`
	// LoadAvg1 is the system 1-minute load average, used together with
	// CPUCount by the dashboard/host-list TUI to render a CPU bar for
	// cloud rentals. Zero values are treated as "unavailable".
	LoadAvg1                 float64             `json:"load_avg_1,omitempty"`
	CPUCount                 int                 `json:"cpu_count,omitempty"`
	DiskFreeBytes            int64               `json:"disk_free_bytes"`
	DiskTotalBytes           int64               `json:"disk_total_bytes"`
	AgentPID                 int                 `json:"agent_pid,omitempty"`
	AgentAlive               *bool               `json:"agent_alive,omitempty"`
	AgentFatal               string              `json:"agent_fatal,omitempty"`
	AgentProtocol            int                 `json:"agent_protocol"`
	OOMScoreAdj              int                 `json:"oom_score_adj"`
	R2PutFailuresConsecutive int                 `json:"r2_put_failures_consecutive,omitempty"`
	R2PutLastError           string              `json:"r2_put_last_error,omitempty"`
	Publication              publicationSnapshot `json:"publication"`
}

// collectHeartbeat gathers host-level metrics and returns a HeartbeatSample.
func collectHeartbeat(phase, diskPath string) HeartbeatSample {
	return collectHeartbeatWithPublication(phase, diskPath, nil)
}

func collectHeartbeatWithPublication(phase, diskPath string, publication *publicationState) HeartbeatSample {
	gpu := runner.HostGPUMetrics()
	memTotal, memUsed, memAvailable := runner.HostMemoryStatsKB()
	diskUsed, diskTotal := probeDiskUsageAtPath(diskPath)

	return HeartbeatSample{
		Ts:             time.Now().Unix(),
		Phase:          phase,
		GPUUtilPct:     gpu.UtilPct,
		GPUMemUsedMiB:  gpu.MemUsedMiB,
		GPUMemTotalMiB: gpu.MemTotalMiB,
		GPUTempC:       gpu.TempC,
		HostRSSKB:      memUsed,
		HostMemTotalKB: memTotal,
		MemAvailableKB: memAvailable,
		LoadAvg1:       runner.HostLoadAvg1(),
		CPUCount:       runner.DetectCPUCount(),
		DiskFreeBytes:  diskTotal - diskUsed,
		DiskTotalBytes: diskTotal,
		AgentProtocol:  controlplane.AgentProtocolVersion,
		Publication:    publication.get(),
	}
}

func withAgentLiveness(sample HeartbeatSample, pid int, alive bool) HeartbeatSample {
	sample.AgentPID = pid
	sample.AgentAlive = &alive
	sample.OOMScoreAdj = readOOMScoreAdj(pid)
	return sample
}

func readOOMScoreAdj(pid int) int {
	if pid <= 0 {
		return 0
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/oom_score_adj")
	if err != nil {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return value
}

func probeDiskUsageAtPath(path string) (used, total int64) {
	if path == "" {
		path = "/"
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return runner.ProbeDiskUsage()
	}
	bsize := runner.StatfsBlockBytes(stat)
	total = int64(stat.Blocks) * bsize
	free := int64(stat.Bavail) * bsize
	return total - free, total
}
