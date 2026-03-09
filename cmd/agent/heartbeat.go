package main

import (
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
}

// collectHeartbeat gathers host-level metrics and returns a HeartbeatSample.
func collectHeartbeat(phase string) HeartbeatSample {
	gpu := runner.HostGPUMetrics()
	memTotal, memUsed := runner.HostMemoryKB()
	diskUsed, diskTotal := runner.ProbeDiskUsage()

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
