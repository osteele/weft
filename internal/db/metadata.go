package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// JobMetadata stores optional derived or cached metadata for a job.
type JobMetadata struct {
	CPU      *JobCPUStats   `json:"cpu,omitempty"`
	Resource *ResourceUsage `json:"resource,omitempty"`
}

// ResourceUsage stores resource consumption captured when a job completes.
type ResourceUsage struct {
	UserCPUSecs  *float64 `json:"user_cpu_secs,omitempty"`
	SysCPUSecs   *float64 `json:"sys_cpu_secs,omitempty"`
	PeakRSSKB    *int64   `json:"peak_rss_kb,omitempty"`
	MaxGPUMemMiB *int64   `json:"max_gpu_mem_mib,omitempty"`
}

// JobCPUStats summarizes CPU samples as percent of total cores.
type JobCPUStats struct {
	Latest  *float64 `json:"latest,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Mean    *float64 `json:"mean,omitempty"`
	Stddev  *float64 `json:"stddev,omitempty"`
	Samples int      `json:"samples,omitempty"`
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
