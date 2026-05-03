package main

import (
	"syscall"
	"time"

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
	DiskFreeBytes  int64  `json:"disk_free_bytes"`
	DiskTotalBytes int64  `json:"disk_total_bytes"`
	AgentPID       int    `json:"agent_pid,omitempty"`
	AgentAlive     *bool  `json:"agent_alive,omitempty"`
	AgentFatal     string `json:"agent_fatal,omitempty"`
}

// collectHeartbeat gathers host-level metrics and returns a HeartbeatSample.
func collectHeartbeat(phase, diskPath string) HeartbeatSample {
	gpu := runner.HostGPUMetrics()
	memTotal, memUsed := runner.HostMemoryKB()
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
		DiskFreeBytes:  diskTotal - diskUsed,
		DiskTotalBytes: diskTotal,
	}
}

func withAgentLiveness(sample HeartbeatSample, pid int, alive bool) HeartbeatSample {
	sample.AgentPID = pid
	sample.AgentAlive = &alive
	return sample
}

func probeDiskUsageAtPath(path string) (used, total int64) {
	if path == "" {
		path = "/"
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return runner.ProbeDiskUsage()
	}
	bsize := int64(stat.Bsize)
	total = int64(stat.Blocks) * bsize
	free := int64(stat.Bavail) * bsize
	return total - free, total
}
