package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var (
	exportOutput string
	exportSince  string
)

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export data for external tools",
}

var exportTrainingDataCmd = &cobra.Command{
	Use:   "training-data",
	Short: "Export per-run data with time series for ML training",
	Long: `Export terminal job runs as JSONL, including time series telemetry.

Each line is a JSON object with run metadata and an embedded timeseries array.
This format is designed for consumption by job-estimator and similar ML tools.

Examples:
 weft export training-data --output training-data.jsonl
  weft export training-data --output training-data.jsonl --since 2025-01-01
  weft export training-data --output training-data.jsonl --since "36h ago"`,
	RunE: runExportTrainingData,
}

func init() {
	rootCmd.AddCommand(exportCmd)
	exportCmd.AddCommand(exportTrainingDataCmd)
	exportTrainingDataCmd.Flags().StringVarP(&exportOutput, "output", "o", "", "Output file (default: stdout)")
	exportTrainingDataCmd.Flags().StringVar(&exportSince, "since", "", "Only include jobs started after this cutoff (YYYY-MM-DD, RFC3339, or duration like \"24h ago\")")
}

// trainingDataRecord is the JSONL output format for job-estimator.
type trainingDataRecord struct {
	RunID               int64                 `json:"run_id"`
	JobID               int64                 `json:"job_id"`
	Host                string                `json:"host"`
	WorkingDir          string                `json:"working_dir,omitempty"`
	Command             string                `json:"command"`
	Project             string                `json:"project,omitempty"`
	Tags                []string              `json:"tags,omitempty"`
	GPUClass            string                `json:"gpu_class,omitempty"`
	RequestedGPU        string                `json:"requested_gpu,omitempty"`
	RequestedGPUClass   string                `json:"requested_gpu_class,omitempty"`
	CPUAllotment        *int                  `json:"cpu_allotment,omitempty"`
	GPUMemGB            *int                  `json:"gpu_mem_gb,omitempty"`
	Backend             string                `json:"backend"`
	Tenant              string                `json:"tenant"`
	Status              string                `json:"status"`
	CloudOutcome        string                `json:"cloud_outcome,omitempty"`
	TerminalOutcome     string                `json:"terminal_outcome,omitempty"`
	Censored            bool                  `json:"censored,omitempty"`
	StartTime           int64                 `json:"start_time"`
	EndTime             int64                 `json:"end_time"`
	DurationS           int64                 `json:"duration_s"`
	ExitCode            int                   `json:"exit_code"`
	FailureReason       string                `json:"failure_reason,omitempty"`
	ErrorDiagnosis      string                `json:"error_diagnosis,omitempty"`
	CPUCount            int                   `json:"cpu_count,omitempty"`
	CPUModel            string                `json:"cpu_model,omitempty"`
	CPUFreq             string                `json:"cpu_freq,omitempty"`
	MemTotal            string                `json:"mem_total,omitempty"`
	ActualGPUName       string                `json:"actual_gpu_name,omitempty"`
	GPUNames            []string              `json:"gpu_names,omitempty"`
	GPUCount            int                   `json:"gpu_count,omitempty"`
	GPUVRAMPerDeviceMiB int                   `json:"gpu_vram_per_device_mib,omitempty"`
	GPUVRAMTotalMiB     int                   `json:"gpu_vram_total_mib,omitempty"`
	ActualGPUClass      string                `json:"actual_gpu_class,omitempty"`
	JobMetadata         *db.JobMetadata       `json:"job_metadata,omitempty"`
	PlacementMeta       *db.PlacementMeta     `json:"placement_meta,omitempty"`
	PeakRSSKB           int64                 `json:"peak_rss_kb,omitempty"`
	MaxGPUMiB           int64                 `json:"max_gpu_mem_mib,omitempty"`
	CPUMean             float64               `json:"cpu_mean,omitempty"`
	SetupDurationS      float64               `json:"setup_duration_s,omitempty"`
	UploadDurationS     float64               `json:"upload_duration_s,omitempty"`
	WrapperToStartS     float64               `json:"wrapper_to_start_s,omitempty"`
	AssignedGPUIndices  []string              `json:"assigned_gpu_indices,omitempty"`
	HostSpecs           *trainingHostSpecs    `json:"host_specs,omitempty"`
	Timeseries          []db.TimeseriesSample `json:"timeseries,omitempty"`
	TelemetryV2         *trainingTelemetryV2  `json:"telemetry_v2,omitempty"`
}

type trainingTelemetryV2 struct {
	Summary *db.JobTelemetrySummary `json:"summary,omitempty"`
	Samples []db.TelemetrySample    `json:"samples,omitempty"`
}

type trainingHostSpecs struct {
	CPUCount            int      `json:"cpu_count,omitempty"`
	CPUModel            string   `json:"cpu_model,omitempty"`
	CPUFreq             string   `json:"cpu_freq,omitempty"`
	MemTotal            string   `json:"mem_total,omitempty"`
	GPUNames            []string `json:"gpu_names,omitempty"`
	GPUCount            int      `json:"gpu_count,omitempty"`
	GPUVRAMPerDeviceMiB int      `json:"gpu_vram_per_device_mib,omitempty"`
	GPUVRAMTotalMiB     int      `json:"gpu_vram_total_mib,omitempty"`
}

func runExportTrainingData(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var sinceTime time.Time
	if exportSince != "" {
		sinceTime, err = parseSinceCutoff(exportSince, time.Now())
		if err != nil {
			return err
		}
	}

	// Open output
	out := os.Stdout
	if exportOutput != "" {
		f, err := os.Create(exportOutput)
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}
		defer f.Close()
		out = f
	}

	var sinceUnix int64
	if !sinceTime.IsZero() {
		sinceUnix = sinceTime.Unix()
	}
	runs, err := db.ListTrainingJobRuns(database, sinceUnix)
	if err != nil {
		return fmt.Errorf("list training job runs: %w", err)
	}

	encoder := json.NewEncoder(out)
	var count int

	for _, run := range runs {
		rec := trainingDataRecord{
			RunID:               run.RunID,
			JobID:               run.JobID,
			Host:                run.Host,
			WorkingDir:          run.WorkingDir,
			Command:             run.Command,
			Project:             run.Project,
			Tags:                append([]string(nil), run.Tags...),
			GPUClass:            run.GPUClass,
			RequestedGPU:        run.RequestedGPU,
			RequestedGPUClass:   run.RequestedGPUClass,
			CPUAllotment:        cloneIntPtr(run.CPUAllotment),
			GPUMemGB:            cloneIntPtr(run.GPUMemGB),
			Backend:             run.Backend,
			Tenant:              run.Tenant,
			Status:              run.Status,
			CloudOutcome:        run.CloudOutcome,
			TerminalOutcome:     run.TerminalOutcome,
			Censored:            run.Censored,
			StartTime:           run.StartTime,
			EndTime:             run.EndTime,
			DurationS:           run.DurationS,
			ExitCode:            run.ExitCode,
			FailureReason:       run.FailureReason,
			ErrorDiagnosis:      run.ErrorDiagnosis,
			CPUCount:            run.CPUCount,
			CPUModel:            run.CPUModel,
			CPUFreq:             run.CPUFreq,
			MemTotal:            run.MemTotal,
			ActualGPUName:       run.ActualGPUName,
			GPUNames:            append([]string(nil), run.GPUNames...),
			GPUCount:            run.GPUCount,
			GPUVRAMPerDeviceMiB: run.GPUVRAMPerDeviceMiB,
			GPUVRAMTotalMiB:     run.GPUVRAMTotalMiB,
			ActualGPUClass:      run.ActualGPUClass,
			JobMetadata:         run.Metadata,
			PlacementMeta:       run.PlacementMeta,
			PeakRSSKB:           run.PeakRSSKB,
			MaxGPUMiB:           run.MaxGPUMemMiB,
			CPUMean:             run.CPUMean,
			SetupDurationS:      run.SetupDurationS,
			UploadDurationS:     run.UploadDurationS,
			WrapperToStartS:     run.WrapperToStartS,
			HostSpecs:           runtimeHostSpecsFromRun(run),
		}

		if rec.JobMetadata != nil && rec.JobMetadata.Resource != nil {
			ru := rec.JobMetadata.Resource
			if rec.PeakRSSKB == 0 && ru.PeakRSSKB != nil {
				rec.PeakRSSKB = *ru.PeakRSSKB
			}
			if rec.MaxGPUMiB == 0 && ru.MaxGPUMemMiB != nil {
				rec.MaxGPUMiB = *ru.MaxGPUMemMiB
			}
		}
		if rec.JobMetadata != nil && rec.JobMetadata.CPU != nil && rec.JobMetadata.CPU.Mean != nil && rec.CPUMean == 0 {
			rec.CPUMean = *rec.JobMetadata.CPU.Mean
		}
		if rec.JobMetadata != nil {
			if rec.JobMetadata.Telemetry != nil {
				rec.AssignedGPUIndices = append([]string(nil), rec.JobMetadata.Telemetry.AssignedGPUIndices...)
				rec.TelemetryV2 = &trainingTelemetryV2{Summary: rec.JobMetadata.Telemetry}
			} else if rec.JobMetadata.Resource != nil && rec.JobMetadata.Resource.GPUDevices != "" {
				rec.AssignedGPUIndices = splitCSV(rec.JobMetadata.Resource.GPUDevices)
			}
		}

		// Fetch timeseries
		ts, err := db.GetTimeseriesByRun(database, run.RunID)
		if err == nil && len(ts) > 0 {
			rec.Timeseries = ts
		}
		if telemetrySamples, err := db.GetTelemetryByRun(database, run.RunID); err == nil && len(telemetrySamples) > 0 {
			if rec.TelemetryV2 == nil {
				rec.TelemetryV2 = &trainingTelemetryV2{}
			}
			rec.TelemetryV2.Samples = telemetrySamples
		}
		if rec.TelemetryV2 != nil && rec.TelemetryV2.Summary == nil && len(rec.TelemetryV2.Samples) == 0 {
			rec.TelemetryV2 = nil
		}

		if err := encoder.Encode(rec); err != nil {
			return fmt.Errorf("encode run %d: %w", run.RunID, err)
		}
		count++
	}

	if exportOutput != "" {
		fmt.Fprintf(os.Stderr, "Exported %d jobs to %s\n", count, exportOutput)
	}
	return nil
}

func runtimeHostSpecsFromRun(run db.TrainingJobRun) *trainingHostSpecs {
	if run.CPUCount == 0 &&
		run.CPUModel == "" &&
		run.CPUFreq == "" &&
		run.MemTotal == "" &&
		len(run.GPUNames) == 0 &&
		run.GPUCount == 0 &&
		run.GPUVRAMPerDeviceMiB == 0 &&
		run.GPUVRAMTotalMiB == 0 {
		return nil
	}

	return &trainingHostSpecs{
		CPUCount:            run.CPUCount,
		CPUModel:            run.CPUModel,
		CPUFreq:             run.CPUFreq,
		MemTotal:            run.MemTotal,
		GPUNames:            append([]string(nil), run.GPUNames...),
		GPUCount:            run.GPUCount,
		GPUVRAMPerDeviceMiB: run.GPUVRAMPerDeviceMiB,
		GPUVRAMTotalMiB:     run.GPUVRAMTotalMiB,
	}
}

func cloneIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func splitCSV(value string) []string {
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
