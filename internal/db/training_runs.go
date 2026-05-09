package db

import (
	"database/sql"
	"fmt"
)

// TrainingJobRun is a stable run-based record for predictor training and export.
type TrainingJobRun struct {
	RunID               int64
	JobID               int64
	Host                string
	WorkingDir          string
	Command             string
	Project             string
	Tags                []string
	GPUClass            string
	RequestedGPU        string
	RequestedGPUClass   string
	CPUAllotment        *int
	GPUMemGB            *int
	Backend             string
	Tenant              string
	Status              string
	CloudOutcome        string
	TerminalOutcome     string
	Censored            bool
	StartTime           int64
	EndTime             int64
	DurationS           int64
	ExitCode            int
	FailureReason       string
	ErrorDiagnosis      string
	CPUCount            int
	CPUModel            string
	CPUFreq             string
	MemTotal            string
	ActualGPUName       string
	GPUNames            []string
	GPUCount            int
	GPUVRAMPerDeviceMiB int
	GPUVRAMTotalMiB     int
	ActualGPUClass      string
	Metadata            *JobMetadata
	PlacementMeta       *PlacementMeta
	PeakRSSKB           int64
	MaxGPUMemMiB        int64
	CPUMean             float64
	SetupDurationS      float64
	UploadDurationS     float64
	WrapperToStartS     float64
}

// ListTrainingJobRuns returns terminal execution attempts ordered by start time.
func ListTrainingJobRuns(db *sql.DB, sinceUnix int64) ([]TrainingJobRun, error) {
	query := `
		SELECT
			run_id,
			job_id,
			host,
			working_dir,
			command,
			project,
			tags,
			requested_gpu,
			requested_gpu_class,
			cpu_allotment,
			gpu_mem_gb,
			backend,
			tenant,
			status,
			cloud_outcome,
			terminal_outcome,
			COALESCE(censored, 0),
			start_time,
			end_time,
			COALESCE(duration_s, 0),
			COALESCE(exit_code, 0),
			cpu_count,
			cpu_model,
			cpu_freq,
			mem_total,
			actual_gpu_name,
			gpu_names,
			COALESCE(gpu_count, 0),
			COALESCE(gpu_vram_per_device_mib, 0),
			COALESCE(gpu_vram_total_mib, 0),
			actual_gpu_class,
			failure_reason,
			error_diagnosis,
			job_metadata,
			placement_meta,
			peak_rss_kb,
			max_gpu_mem_mib,
			cpu_mean,
			setup_duration_s,
			upload_duration_s,
			wrapper_to_start_s
		FROM training_examples
	`
	var args []any
	if sinceUnix > 0 {
		query += ` WHERE start_time >= ?`
		args = append(args, sinceUnix)
	}
	query += ` ORDER BY start_time ASC, run_id ASC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query training job runs: %w", err)
	}
	defer rows.Close()

	var runs []TrainingJobRun
	for rows.Next() {
		var run TrainingJobRun
		var workingDir, project, tags, requestedGPU, requestedGPUClass sql.NullString
		var backend, tenant, status sql.NullString
		var cloudOutcome, terminalOutcome sql.NullString
		var censored sql.NullBool
		var cpuAllotment, gpuMemGB, cpuCount sql.NullInt64
		var cpuModel, cpuFreq, memTotal sql.NullString
		var actualGPUName, gpuNames, actualGPUClass sql.NullString
		var failureReason, errorDiagnosis, jobMetadata, placementMeta sql.NullString
		var peakRSSKB, maxGPUMemMiB sql.NullInt64
		var cpuMean sql.NullFloat64
		var setupDurationS, uploadDurationS, wrapperToStartS sql.NullFloat64
		if err := rows.Scan(
			&run.RunID,
			&run.JobID,
			&run.Host,
			&workingDir,
			&run.Command,
			&project,
			&tags,
			&requestedGPU,
			&requestedGPUClass,
			&cpuAllotment,
			&gpuMemGB,
			&backend,
			&tenant,
			&status,
			&cloudOutcome,
			&terminalOutcome,
			&censored,
			&run.StartTime,
			&run.EndTime,
			&run.DurationS,
			&run.ExitCode,
			&cpuCount,
			&cpuModel,
			&cpuFreq,
			&memTotal,
			&actualGPUName,
			&gpuNames,
			&run.GPUCount,
			&run.GPUVRAMPerDeviceMiB,
			&run.GPUVRAMTotalMiB,
			&actualGPUClass,
			&failureReason,
			&errorDiagnosis,
			&jobMetadata,
			&placementMeta,
			&peakRSSKB,
			&maxGPUMemMiB,
			&cpuMean,
			&setupDurationS,
			&uploadDurationS,
			&wrapperToStartS,
		); err != nil {
			return nil, fmt.Errorf("scan training job run: %w", err)
		}
		if workingDir.Valid {
			run.WorkingDir = workingDir.String
		}
		if project.Valid {
			run.Project = project.String
		}
		run.Tags = decodeTags(tags)
		if requestedGPU.Valid {
			run.RequestedGPU = requestedGPU.String
		}
		if requestedGPUClass.Valid {
			run.RequestedGPUClass = requestedGPUClass.String
		}
		run.GPUClass = run.RequestedGPUClass
		if cpuAllotment.Valid {
			value := int(cpuAllotment.Int64)
			run.CPUAllotment = &value
		}
		if gpuMemGB.Valid {
			value := int(gpuMemGB.Int64)
			run.GPUMemGB = &value
		}
		if backend.Valid {
			run.Backend = backend.String
		} else {
			run.Backend = BackendQueueRunner
		}
		if tenant.Valid {
			run.Tenant = tenant.String
		}
		if status.Valid {
			run.Status = status.String
		}
		if cloudOutcome.Valid {
			run.CloudOutcome = cloudOutcome.String
		}
		if terminalOutcome.Valid {
			run.TerminalOutcome = terminalOutcome.String
		}
		if censored.Valid {
			run.Censored = censored.Bool
		}
		if cpuCount.Valid {
			run.CPUCount = int(cpuCount.Int64)
		}
		if cpuModel.Valid {
			run.CPUModel = cpuModel.String
		}
		if cpuFreq.Valid {
			run.CPUFreq = cpuFreq.String
		}
		if memTotal.Valid {
			run.MemTotal = memTotal.String
		}
		if actualGPUName.Valid {
			run.ActualGPUName = actualGPUName.String
		}
		run.GPUNames = decodeStringSlice(gpuNames)
		if actualGPUClass.Valid {
			run.ActualGPUClass = actualGPUClass.String
		}
		if failureReason.Valid {
			run.FailureReason = failureReason.String
		}
		if errorDiagnosis.Valid {
			run.ErrorDiagnosis = errorDiagnosis.String
		}
		run.Metadata = decodeJobMetadata(jobMetadata)
		run.PlacementMeta = decodePlacementMeta(placementMeta)
		if peakRSSKB.Valid {
			run.PeakRSSKB = peakRSSKB.Int64
		}
		if maxGPUMemMiB.Valid {
			run.MaxGPUMemMiB = maxGPUMemMiB.Int64
		}
		if cpuMean.Valid {
			run.CPUMean = cpuMean.Float64
		}
		if setupDurationS.Valid {
			run.SetupDurationS = setupDurationS.Float64
		}
		if uploadDurationS.Valid {
			run.UploadDurationS = uploadDurationS.Float64
		}
		if wrapperToStartS.Valid {
			run.WrapperToStartS = wrapperToStartS.Float64
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
