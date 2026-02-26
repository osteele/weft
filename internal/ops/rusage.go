package ops

import (
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
)

// parseResourceUsage parses key=value content from a .rusage file into a ResourceUsage struct.
// Returns nil if there are no valid fields.
func parseResourceUsage(content string) *db.ResourceUsage {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	var ru db.ResourceUsage
	hasField := false

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if value == "" {
			continue
		}

		switch key {
		case "user_cpu_secs":
			if v, err := strconv.ParseFloat(value, 64); err == nil {
				ru.UserCPUSecs = &v
				hasField = true
			}
		case "sys_cpu_secs":
			if v, err := strconv.ParseFloat(value, 64); err == nil {
				ru.SysCPUSecs = &v
				hasField = true
			}
		case "peak_rss_kb":
			if v, err := strconv.ParseInt(value, 10, 64); err == nil {
				ru.PeakRSSKB = &v
				hasField = true
			}
		case "max_gpu_mem_mib":
			if v, err := strconv.ParseInt(value, 10, 64); err == nil {
				ru.MaxGPUMemMiB = &v
				hasField = true
			}
		case "gpu_devices":
			ru.GPUDevices = value
			hasField = true
		}
	}

	if !hasField {
		return nil
	}
	return &ru
}

// updateJobResourceUsage fetches and stores the resource usage data for a completed job.
// Returns true if the metadata was updated.
func updateJobResourceUsage(database *sql.DB, job *db.Job, timeout time.Duration) (bool, error) {
	if job == nil {
		return false, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	content, err := queueRemoteClient.Rusage(job.Host, job.ID, timeout)
	if err != nil {
		return false, nil // best effort
	}

	ru := parseResourceUsage(content)
	if ru == nil {
		return false, nil
	}

	meta := job.Metadata
	if meta == nil {
		meta = &db.JobMetadata{}
	}
	meta.Resource = ru
	if err := db.SetJobMetadata(database, job.ID, meta); err != nil {
		return false, err
	}
	job.Metadata = meta
	return true, nil
}
