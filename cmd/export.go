package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
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
  weft export training-data --output training-data.jsonl --since 2025-01-01`,
	RunE: runExportTrainingData,
}

func init() {
	rootCmd.AddCommand(exportCmd)
	exportCmd.AddCommand(exportTrainingDataCmd)
	exportTrainingDataCmd.Flags().StringVarP(&exportOutput, "output", "o", "", "Output file (default: stdout)")
	exportTrainingDataCmd.Flags().StringVar(&exportSince, "since", "", "Only include jobs started after this date (YYYY-MM-DD)")
}

// trainingDataRecord is the JSONL output format for job-estimator.
type trainingDataRecord struct {
	RunID              int64                 `json:"run_id"`
	JobID              int64                 `json:"job_id"`
	Host               string                `json:"host"`
	WorkingDir         string                `json:"working_dir,omitempty"`
	Command            string                `json:"command"`
	Project            string                `json:"project,omitempty"`
	GPUClass           string                `json:"gpu_class,omitempty"`
	Backend            string                `json:"backend"`
	Tenant             string                `json:"tenant"`
	StartTime          int64                 `json:"start_time"`
	EndTime            int64                 `json:"end_time"`
	DurationS          int64                 `json:"duration_s"`
	ExitCode           int                   `json:"exit_code"`
	FailureReason      string                `json:"failure_reason,omitempty"`
	ErrorDiagnosis     string                `json:"error_diagnosis,omitempty"`
	PeakRSSKB          int64                 `json:"peak_rss_kb,omitempty"`
	MaxGPUMiB          int64                 `json:"max_gpu_mem_mib,omitempty"`
	CPUMean            float64               `json:"cpu_mean,omitempty"`
	AssignedGPUIndices []string              `json:"assigned_gpu_indices,omitempty"`
	HostSpecs          *trainingHostSpecs    `json:"host_specs,omitempty"`
	Timeseries         []db.TimeseriesSample `json:"timeseries,omitempty"`
	TelemetryV2        *trainingTelemetryV2  `json:"telemetry_v2,omitempty"`
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
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	hostSpecs, err := loadTrainingHostSpecs(database)
	if err != nil {
		return fmt.Errorf("load host specs: %w", err)
	}

	// Parse since filter
	var sinceTime time.Time
	if exportSince != "" {
		sinceTime, err = time.Parse("2006-01-02", exportSince)
		if err != nil {
			return fmt.Errorf("invalid --since date (use YYYY-MM-DD): %w", err)
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
			RunID:          run.RunID,
			JobID:          run.JobID,
			Host:           run.Host,
			WorkingDir:     run.WorkingDir,
			Command:        run.Command,
			Project:        run.Project,
			GPUClass:       run.GPUClass,
			Backend:        run.Backend,
			Tenant:         run.Tenant,
			StartTime:      run.StartTime,
			EndTime:        run.EndTime,
			DurationS:      run.DurationS,
			ExitCode:       run.ExitCode,
			FailureReason:  run.FailureReason,
			ErrorDiagnosis: run.ErrorDiagnosis,
			HostSpecs:      hostSpecs[run.Host],
		}

		// Extract resource usage from metadata
		if run.Metadata != nil && run.Metadata.Resource != nil {
			ru := run.Metadata.Resource
			if ru.PeakRSSKB != nil {
				rec.PeakRSSKB = *ru.PeakRSSKB
			}
			if ru.MaxGPUMemMiB != nil {
				rec.MaxGPUMiB = *ru.MaxGPUMemMiB
			}
		}
		if run.Metadata != nil && run.Metadata.CPU != nil && run.Metadata.CPU.Mean != nil {
			rec.CPUMean = *run.Metadata.CPU.Mean
		}
		if run.Metadata != nil {
			if run.Metadata.Telemetry != nil {
				rec.AssignedGPUIndices = append([]string(nil), run.Metadata.Telemetry.AssignedGPUIndices...)
				rec.TelemetryV2 = &trainingTelemetryV2{Summary: run.Metadata.Telemetry}
			} else if run.Metadata.Resource != nil && run.Metadata.Resource.GPUDevices != "" {
				rec.AssignedGPUIndices = splitCSV(run.Metadata.Resource.GPUDevices)
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

func loadTrainingHostSpecs(database *sql.DB) (map[string]*trainingHostSpecs, error) {
	cachedHosts, err := db.LoadAllCachedHosts(database)
	if err != nil {
		return nil, err
	}

	specs := make(map[string]*trainingHostSpecs, len(cachedHosts))
	for _, cached := range cachedHosts {
		specs[cached.Name] = cachedHostToTrainingSpecs(cached)
	}
	return specs, nil
}

func cachedHostToTrainingSpecs(cached *db.CachedHostInfo) *trainingHostSpecs {
	if cached == nil {
		return nil
	}

	result := &trainingHostSpecs{
		CPUCount: cached.CPUCount,
		CPUModel: cached.CPUModel,
		CPUFreq:  cached.CPUFreq,
		MemTotal: cached.MemTotal,
	}

	var gpus []hostinfo.GPUInfo
	if err := json.Unmarshal([]byte(cached.GPUsJSON), &gpus); err != nil {
		return result
	}

	result.GPUCount = len(gpus)
	result.GPUNames = make([]string, 0, len(gpus))
	for _, gpu := range gpus {
		if gpu.Name != "" {
			result.GPUNames = append(result.GPUNames, gpu.Name)
		}
		vram := parseMiB(gpu.MemTotal)
		if vram > 0 {
			result.GPUVRAMTotalMiB += vram
			if result.GPUVRAMPerDeviceMiB == 0 {
				result.GPUVRAMPerDeviceMiB = vram
			}
		}
	}

	return result
}

func parseMiB(value string) int {
	cleaned := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(value, "MiB"), "MB"))
	if cleaned == "" {
		return 0
	}
	n, err := strconv.Atoi(cleaned)
	if err != nil {
		return 0
	}
	return n
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
