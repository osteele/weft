package ops

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
)

// syncJobTelemetry fetches richer telemetry JSONL from a remote host and imports
// any new samples into the local telemetry tables.
func syncJobTelemetry(database *sql.DB, job *db.Job, timeout time.Duration) error {
	if job == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	lastTS, err := db.GetTelemetryLastTS(database, job.ID)
	if err != nil {
		return fmt.Errorf("get telemetry last ts: %w", err)
	}

	cmd := fmt.Sprintf("cat %s 2>/dev/null", session.SimpleTelemetryFile(job.ID))
	stdout, _, err := ssh.RunWithTimeout(job.Host, cmd, timeout)
	if err != nil || strings.TrimSpace(stdout) == "" {
		return nil
	}

	samples := db.ParseTelemetrySamplesJSONL(stdout, lastTS)

	if len(samples) == 0 {
		return nil
	}
	if err := db.InsertTelemetrySamples(database, job.ID, samples); err != nil {
		return fmt.Errorf("insert telemetry: %w", err)
	}
	if err := db.RefreshJobTelemetrySummary(database, job.ID); err != nil {
		return fmt.Errorf("refresh telemetry summary: %w", err)
	}
	if err := upsertTelemetryPhaseMetrics(database, job, samples); err != nil {
		return fmt.Errorf("upsert telemetry phase metrics: %w", err)
	}
	return nil
}

func upsertTelemetryPhaseMetrics(database *sql.DB, job *db.Job, samples []db.TelemetrySample) error {
	if database == nil || job == nil || len(samples) == 0 {
		return nil
	}
	var peakMem int
	var utilSum float64
	var utilCount int
	var peakUtil int
	for _, sample := range samples {
		for _, gpu := range sample.GPUs {
			if gpu.GPUMemUsedMiB > peakMem {
				peakMem = gpu.GPUMemUsedMiB
			}
			if gpu.GPUUtilPct != nil {
				utilSum += *gpu.GPUUtilPct
				utilCount++
				if int(*gpu.GPUUtilPct) > peakUtil {
					peakUtil = int(*gpu.GPUUtilPct)
				}
			}
		}
	}
	t := &db.JobPhaseTimings{JobID: job.ID}
	if peakMem > 0 {
		t.PeakGPUMemMiB = &peakMem
	}
	if utilCount > 0 {
		mean := int(utilSum / float64(utilCount))
		t.MeanGPUUtil = &mean
	}
	if peakUtil > 0 {
		t.PeakGPUUtil = &peakUtil
	}
	if t.PeakGPUMemMiB == nil && t.MeanGPUUtil == nil && t.PeakGPUUtil == nil {
		return nil
	}
	return db.UpsertJobPhaseTimings(database, t)
}
