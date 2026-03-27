package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// TelemetrySample represents one raw telemetry sample for a job run.
type TelemetrySample struct {
	JobRunID         *int64               `json:"-"`
	Ts               int64                `json:"ts"`
	ElapsedS         float64              `json:"elapsed_s,omitempty"`
	ProcCPUUserS     float64              `json:"proc_cpu_user_s,omitempty"`
	ProcCPUSysS      float64              `json:"proc_cpu_sys_s,omitempty"`
	ProcRSSKB        int64                `json:"proc_rss_kb,omitempty"`
	HostCPUUtilPct   *float64             `json:"host_cpu_util_pct,omitempty"`
	ProcDiskReadBPS  *float64             `json:"proc_disk_read_bps,omitempty"`
	ProcDiskWriteBPS *float64             `json:"proc_disk_write_bps,omitempty"`
	ProcNetRxBPS     *float64             `json:"proc_net_rx_bps,omitempty"`
	ProcNetTxBPS     *float64             `json:"proc_net_tx_bps,omitempty"`
	GPUs             []TelemetryGPUSample `json:"gpus,omitempty"`
}

// TelemetryGPUSample represents one GPU's telemetry at a timestamp.
type TelemetryGPUSample struct {
	GPUIndex       string   `json:"gpu_index"`
	GPUName        string   `json:"gpu_name,omitempty"`
	GPUMemUsedMiB  int      `json:"gpu_mem_used_mib,omitempty"`
	GPUUtilPct     *float64 `json:"gpu_util_pct,omitempty"`
	GPUMemUtilPct  *float64 `json:"gpu_mem_util_pct,omitempty"`
	GPUPowerW      *float64 `json:"gpu_power_w,omitempty"`
	GPUPCIeTxMiBS  *float64 `json:"gpu_pcie_tx_mib_s,omitempty"`
	GPUPCIeRxMiBS  *float64 `json:"gpu_pcie_rx_mib_s,omitempty"`
	GPUSMClockMHz  *uint32  `json:"gpu_sm_clock_mhz,omitempty"`
	GPUMemClockMHz *uint32  `json:"gpu_mem_clock_mhz,omitempty"`
}

// InsertTelemetrySamples bulk-inserts telemetry for a job's latest run.
func InsertTelemetrySamples(database *sql.DB, jobID int64, samples []TelemetrySample) error {
	if len(samples) == 0 {
		return nil
	}
	runID, err := latestRunIDForJob(database, jobID)
	if err != nil {
		return fmt.Errorf("resolve latest run: %w", err)
	}
	if runID == nil {
		return nil
	}

	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	sampleStmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO job_telemetry_samples (
			job_id, attempt_id, ts, elapsed_s, proc_cpu_user_s, proc_cpu_sys_s, proc_rss_kb,
			host_cpu_util_pct, proc_disk_read_bps, proc_disk_write_bps, proc_net_rx_bps, proc_net_tx_bps
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare sample insert: %w", err)
	}
	defer sampleStmt.Close()

	gpuStmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO job_telemetry_gpus (
			job_id, attempt_id, ts, gpu_index, gpu_name, gpu_mem_used_mib, gpu_util_pct,
			gpu_mem_util_pct, gpu_power_w, gpu_pcie_tx_mib_s, gpu_pcie_rx_mib_s,
			gpu_sm_clock_mhz, gpu_mem_clock_mhz
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare gpu insert: %w", err)
	}
	defer gpuStmt.Close()

	for _, sample := range samples {
		sample.JobRunID = runID
		if _, err := sampleStmt.Exec(
			jobID, *runID, sample.Ts, sample.ElapsedS, sample.ProcCPUUserS, sample.ProcCPUSysS, sample.ProcRSSKB,
			nullableFloatPtr(sample.HostCPUUtilPct), nullableFloatPtr(sample.ProcDiskReadBPS), nullableFloatPtr(sample.ProcDiskWriteBPS),
			nullableFloatPtr(sample.ProcNetRxBPS), nullableFloatPtr(sample.ProcNetTxBPS),
		); err != nil {
			return fmt.Errorf("insert telemetry sample: %w", err)
		}
		for _, gpu := range sample.GPUs {
			if _, err := gpuStmt.Exec(
				jobID, *runID, sample.Ts, gpu.GPUIndex, nullIfEmpty(gpu.GPUName), gpu.GPUMemUsedMiB,
				nullableFloatPtr(gpu.GPUUtilPct), nullableFloatPtr(gpu.GPUMemUtilPct), nullableFloatPtr(gpu.GPUPowerW),
				nullableFloatPtr(gpu.GPUPCIeTxMiBS), nullableFloatPtr(gpu.GPUPCIeRxMiBS), nullUint32(gpu.GPUSMClockMHz),
				nullUint32(gpu.GPUMemClockMHz),
			); err != nil {
				return fmt.Errorf("insert telemetry gpu sample: %w", err)
			}
		}
	}

	return tx.Commit()
}

// GetTelemetryByRun reads all raw telemetry for a run, ordered by time.
func GetTelemetryByRun(database *sql.DB, runID int64) ([]TelemetrySample, error) {
	rows, err := database.Query(`
		SELECT attempt_id, ts, elapsed_s, proc_cpu_user_s, proc_cpu_sys_s, proc_rss_kb,
		       host_cpu_util_pct, proc_disk_read_bps, proc_disk_write_bps, proc_net_rx_bps, proc_net_tx_bps
		FROM job_telemetry_samples
		WHERE attempt_id = ?
		ORDER BY ts ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("query telemetry samples: %w", err)
	}
	defer rows.Close()

	var samples []TelemetrySample
	byTS := make(map[int64]*TelemetrySample)
	for rows.Next() {
		var (
			s                TelemetrySample
			jobRunID         sql.NullInt64
			hostCPUUtilPct   sql.NullFloat64
			procDiskReadBPS  sql.NullFloat64
			procDiskWriteBPS sql.NullFloat64
			procNetRxBPS     sql.NullFloat64
			procNetTxBPS     sql.NullFloat64
		)
		if err := rows.Scan(
			&jobRunID, &s.Ts, &s.ElapsedS, &s.ProcCPUUserS, &s.ProcCPUSysS, &s.ProcRSSKB,
			&hostCPUUtilPct, &procDiskReadBPS, &procDiskWriteBPS, &procNetRxBPS, &procNetTxBPS,
		); err != nil {
			return nil, fmt.Errorf("scan telemetry sample: %w", err)
		}
		if jobRunID.Valid {
			s.JobRunID = &jobRunID.Int64
		}
		s.HostCPUUtilPct = floatPtrFromNull(hostCPUUtilPct)
		s.ProcDiskReadBPS = floatPtrFromNull(procDiskReadBPS)
		s.ProcDiskWriteBPS = floatPtrFromNull(procDiskWriteBPS)
		s.ProcNetRxBPS = floatPtrFromNull(procNetRxBPS)
		s.ProcNetTxBPS = floatPtrFromNull(procNetTxBPS)
		samples = append(samples, s)
		byTS[s.Ts] = &samples[len(samples)-1]
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	gpuRows, err := database.Query(`
		SELECT ts, gpu_index, gpu_name, gpu_mem_used_mib, gpu_util_pct, gpu_mem_util_pct,
		       gpu_power_w, gpu_pcie_tx_mib_s, gpu_pcie_rx_mib_s, gpu_sm_clock_mhz, gpu_mem_clock_mhz
		FROM job_telemetry_gpus
		WHERE attempt_id = ?
		ORDER BY ts ASC, gpu_index ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("query telemetry gpus: %w", err)
	}
	defer gpuRows.Close()

	for gpuRows.Next() {
		var (
			ts             int64
			gpu            TelemetryGPUSample
			gpuName        sql.NullString
			gpuUtilPct     sql.NullFloat64
			gpuMemUtilPct  sql.NullFloat64
			gpuPowerW      sql.NullFloat64
			gpuPCIeTxMiBS  sql.NullFloat64
			gpuPCIeRxMiBS  sql.NullFloat64
			gpuSMClockMHz  sql.NullInt64
			gpuMemClockMHz sql.NullInt64
		)
		if err := gpuRows.Scan(
			&ts, &gpu.GPUIndex, &gpuName, &gpu.GPUMemUsedMiB, &gpuUtilPct, &gpuMemUtilPct,
			&gpuPowerW, &gpuPCIeTxMiBS, &gpuPCIeRxMiBS, &gpuSMClockMHz, &gpuMemClockMHz,
		); err != nil {
			return nil, fmt.Errorf("scan telemetry gpu: %w", err)
		}
		if gpuName.Valid {
			gpu.GPUName = gpuName.String
		}
		gpu.GPUUtilPct = floatPtrFromNull(gpuUtilPct)
		gpu.GPUMemUtilPct = floatPtrFromNull(gpuMemUtilPct)
		gpu.GPUPowerW = floatPtrFromNull(gpuPowerW)
		gpu.GPUPCIeTxMiBS = floatPtrFromNull(gpuPCIeTxMiBS)
		gpu.GPUPCIeRxMiBS = floatPtrFromNull(gpuPCIeRxMiBS)
		gpu.GPUSMClockMHz = uint32PtrFromNull(gpuSMClockMHz)
		gpu.GPUMemClockMHz = uint32PtrFromNull(gpuMemClockMHz)
		if sample := byTS[ts]; sample != nil {
			sample.GPUs = append(sample.GPUs, gpu)
		}
	}
	return samples, gpuRows.Err()
}

// GetTelemetryLastTS returns the latest telemetry timestamp for a job's latest run.
func GetTelemetryLastTS(database *sql.DB, jobID int64) (int64, error) {
	runID, err := latestRunIDForJob(database, jobID)
	if err != nil {
		return 0, fmt.Errorf("resolve latest run: %w", err)
	}
	if runID == nil {
		return 0, nil
	}
	var ts sql.NullInt64
	if err := database.QueryRow(`SELECT MAX(ts) FROM job_telemetry_samples WHERE attempt_id = ?`, *runID).Scan(&ts); err != nil {
		return 0, fmt.Errorf("query max telemetry ts: %w", err)
	}
	if ts.Valid {
		return ts.Int64, nil
	}
	return 0, nil
}

// RefreshJobTelemetrySummary recomputes the derived telemetry summary for the latest run.
func RefreshJobTelemetrySummary(database *sql.DB, jobID int64) error {
	job, err := GetJobByID(database, jobID)
	if err != nil || job == nil || job.LatestRunID == nil {
		return err
	}

	samples, err := GetTelemetryByRun(database, *job.LatestRunID)
	if err != nil || len(samples) == 0 {
		return err
	}

	var wallDuration float64
	if job.StartTime > 0 && job.EndTime != nil && *job.EndTime > job.StartTime {
		wallDuration = float64(*job.EndTime - job.StartTime)
	}

	assigned := splitCSVStrings("")
	if job.Metadata != nil && job.Metadata.Resource != nil {
		assigned = splitCSVStrings(job.Metadata.Resource.GPUDevices)
	}

	summary := SummarizeTelemetry(samples, wallDuration, assigned)
	if summary == nil {
		return nil
	}

	meta := job.Metadata
	if meta == nil {
		meta = &JobMetadata{}
	}
	meta.Telemetry = summary
	return SetJobMetadata(database, jobID, meta)
}

// SummarizeTelemetry converts raw samples into a compact per-run summary.
func SummarizeTelemetry(samples []TelemetrySample, wallDuration float64, assignedGPUIndices []string) *JobTelemetrySummary {
	if len(samples) == 0 {
		return nil
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i].Ts < samples[j].Ts })
	summary := &JobTelemetrySummary{
		WallDurationS:      wallDuration,
		AssignedGPUIndices: append([]string(nil), assignedGPUIndices...),
	}

	if wallDuration <= 0 {
		if lastElapsed := samples[len(samples)-1].ElapsedS; lastElapsed > 0 {
			summary.WallDurationS = lastElapsed
		}
	}

	last := samples[len(samples)-1]
	summary.ProcCPUUserSFinal = last.ProcCPUUserS
	summary.ProcCPUSysSFinal = last.ProcCPUSysS
	summary.CPUCoreSeconds = last.ProcCPUUserS + last.ProcCPUSysS
	if summary.WallDurationS > 0 {
		summary.MeanCPUCores = summary.CPUCoreSeconds / summary.WallDurationS
	}

	defaultDelta := sampleInterval(samples, summary.WallDurationS)
	deviceAgg := make(map[string]*telemetryDeviceAccumulator)
	seenGPUIndices := make(map[string]struct{})
	for i, sample := range samples {
		if sample.ProcRSSKB > summary.MaxRSSKB {
			summary.MaxRSSKB = sample.ProcRSSKB
		}
		duration := defaultDelta
		if i < len(samples)-1 {
			delta := samples[i+1].ElapsedS - sample.ElapsedS
			if delta > 0 {
				duration = delta
			}
		}
		for _, gpu := range sample.GPUs {
			seenGPUIndices[gpu.GPUIndex] = struct{}{}
			acc := deviceAgg[gpu.GPUIndex]
			if acc == nil {
				acc = &telemetryDeviceAccumulator{name: gpu.GPUName}
				deviceAgg[gpu.GPUIndex] = acc
			}
			if gpu.GPUName != "" {
				acc.name = gpu.GPUName
			}
			if gpu.GPUMemUsedMiB > acc.peakMemMiB {
				acc.peakMemMiB = gpu.GPUMemUsedMiB
			}
			acc.totalDuration += duration
			if gpu.GPUUtilPct != nil {
				acc.utilWeighted += *gpu.GPUUtilPct * duration
				if *gpu.GPUUtilPct > 0 {
					acc.activeSeconds += duration
				}
			}
			if gpu.GPUMemUtilPct != nil {
				acc.memUtilWeighted += *gpu.GPUMemUtilPct * duration
				acc.hasMemUtil = true
				if *gpu.GPUMemUtilPct > 0 && gpu.GPUUtilPct == nil {
					acc.activeSeconds += duration
				}
			}
		}
	}

	if len(summary.AssignedGPUIndices) == 0 {
		for index := range seenGPUIndices {
			summary.AssignedGPUIndices = append(summary.AssignedGPUIndices, index)
		}
		sort.Strings(summary.AssignedGPUIndices)
	}

	for index, acc := range deviceAgg {
		deviceSummary := JobTelemetryDeviceSummary{
			GPUIndex:         index,
			GPUName:          acc.name,
			GPUActiveSeconds: acc.activeSeconds,
			GPUPeakMemMiB:    acc.peakMemMiB,
		}
		if acc.totalDuration > 0 {
			value := acc.utilWeighted / acc.totalDuration
			deviceSummary.GPUMeanUtilPct = &value
			if acc.hasMemUtil {
				memValue := acc.memUtilWeighted / acc.totalDuration
				deviceSummary.GPUMeanMemUtilPct = &memValue
			}
		}
		summary.GPUs = append(summary.GPUs, deviceSummary)
	}
	sort.Slice(summary.GPUs, func(i, j int) bool {
		return summary.GPUs[i].GPUIndex < summary.GPUs[j].GPUIndex
	})

	return summary
}

type telemetryDeviceAccumulator struct {
	name            string
	totalDuration   float64
	activeSeconds   float64
	utilWeighted    float64
	memUtilWeighted float64
	hasMemUtil      bool
	peakMemMiB      int
}

func sampleInterval(samples []TelemetrySample, wallDuration float64) float64 {
	if len(samples) > 1 {
		last := samples[len(samples)-1].ElapsedS - samples[len(samples)-2].ElapsedS
		if last > 0 {
			return last
		}
	}
	if wallDuration > 0 {
		return wallDuration
	}
	return 1
}

func splitCSVStrings(value string) []string {
	parts := strings.Split(value, ",")
	var result []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

// ParseTelemetrySamplesJSONL parses newline-delimited JSON telemetry records,
// returning only samples newer than lastTS.
func ParseTelemetrySamplesJSONL(data string, lastTS int64) []TelemetrySample {
	var samples []TelemetrySample
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var sample TelemetrySample
		if err := json.Unmarshal([]byte(line), &sample); err != nil {
			continue
		}
		if sample.Ts <= lastTS {
			continue
		}
		samples = append(samples, sample)
	}
	return samples
}

func nullableFloatPtr(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullUint32(value *uint32) any {
	if value == nil {
		return nil
	}
	return *value
}

func floatPtrFromNull(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	v := value.Float64
	return &v
}

func uint32PtrFromNull(value sql.NullInt64) *uint32 {
	if !value.Valid {
		return nil
	}
	v := uint32(value.Int64)
	return &v
}
