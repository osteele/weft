package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// JobMetadata stores optional derived or cached metadata for a job.
type JobMetadata struct {
	CPU              *JobCPUStats           `json:"cpu,omitempty"`
	Resource         *ResourceUsage         `json:"resource,omitempty"`
	Telemetry        *JobTelemetrySummary   `json:"telemetry,omitempty"`
	Dependencies     *JobDependencyMetadata `json:"dependencies,omitempty"`
	Disk             *JobDiskMetadata       `json:"disk,omitempty"`
	BestEffortInputs []string               `json:"best_effort_inputs,omitempty"`
}

// JobDiskMetadata stores disk requirements for rental placement. DiskGB is a
// total instance disk floor; RuntimeDiskGB is explicit scratch/cache headroom
// beyond declared inputs and base overhead.
type JobDiskMetadata struct {
	DiskGB        int `json:"disk_gb,omitempty"`
	RuntimeDiskGB int `json:"runtime_disk_gb,omitempty"`
	// EstimatedRuntimeDiskGB is kept for compatibility with older job rows.
	EstimatedRuntimeDiskGB int `json:"estimated_runtime_disk_gb,omitempty"`
}

// JobDependencyMetadata stores dependency semantics that cannot be encoded as
// host-local queue-runner deps (e.g., rental/cloud upstream dependencies).
type JobDependencyMetadata struct {
	CloudAfter []JobDependencyRef `json:"cloud_after,omitempty"`
	CloudNeeds []string           `json:"cloud_needs,omitempty"`
}

// JobDependencyRef identifies a dependency on another logical job.
type JobDependencyRef struct {
	JobID        int64 `json:"job_id"`
	AllowFailure bool  `json:"allow_failure,omitempty"`
}

// ResourceUsage stores resource consumption captured when a job completes.
type ResourceUsage struct {
	UserCPUSecs  *float64 `json:"user_cpu_secs,omitempty"`
	SysCPUSecs   *float64 `json:"sys_cpu_secs,omitempty"`
	PeakRSSKB    *int64   `json:"peak_rss_kb,omitempty"`
	MaxGPUMemMiB *int64   `json:"max_gpu_mem_mib,omitempty"`
	GPUDevices   string   `json:"gpu_devices,omitempty"` // assigned CUDA device indices, e.g. "0" or "0,1"
}

// JobCPUStats summarizes CPU samples as percent of total cores.
type JobCPUStats struct {
	Latest  *float64 `json:"latest,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Mean    *float64 `json:"mean,omitempty"`
	Stddev  *float64 `json:"stddev,omitempty"`
	Samples int      `json:"samples,omitempty"`
}

// JobTelemetrySummary stores derived summary telemetry for a completed job run.
type JobTelemetrySummary struct {
	WallDurationS      float64                     `json:"wall_duration_s,omitempty"`
	ProcCPUUserSFinal  float64                     `json:"proc_cpu_user_s_final,omitempty"`
	ProcCPUSysSFinal   float64                     `json:"proc_cpu_sys_s_final,omitempty"`
	CPUCoreSeconds     float64                     `json:"cpu_core_seconds,omitempty"`
	MeanCPUCores       float64                     `json:"mean_cpu_cores,omitempty"`
	MaxRSSKB           int64                       `json:"max_rss_kb,omitempty"`
	AssignedGPUIndices []string                    `json:"assigned_gpu_indices,omitempty"`
	GPUs               []JobTelemetryDeviceSummary `json:"gpus,omitempty"`
}

// JobTelemetryDeviceSummary stores per-device derived telemetry.
type JobTelemetryDeviceSummary struct {
	GPUIndex          string   `json:"gpu_index"`
	GPUName           string   `json:"gpu_name,omitempty"`
	GPUActiveSeconds  float64  `json:"gpu_active_seconds,omitempty"`
	GPUMeanUtilPct    *float64 `json:"gpu_mean_util_pct,omitempty"`
	GPUMeanMemUtilPct *float64 `json:"gpu_mean_mem_util_pct,omitempty"`
	GPUPeakMemMiB     int      `json:"gpu_peak_mem_mib,omitempty"`
}

func decodeJobMetadata(value sql.NullString) *JobMetadata {
	if !value.Valid {
		return nil
	}
	raw := strings.TrimSpace(value.String)
	if raw == "" {
		return nil
	}
	var meta JobMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return nil
	}
	return &meta
}

func encodeJobMetadata(meta *JobMetadata) (interface{}, error) {
	if meta == nil {
		return nil, nil
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode job metadata: %w", err)
	}
	return string(data), nil
}
