package cmd

import (
	"encoding/json"
	"fmt"
	"os"
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
	RunID      int64                 `json:"run_id"`
	JobID      int64                 `json:"job_id"`
	Host       string                `json:"host"`
	Command    string                `json:"command"`
	Project    string                `json:"project,omitempty"`
	GPUClass   string                `json:"gpu_class,omitempty"`
	Backend    string                `json:"backend"`
	Tenant     string                `json:"tenant"`
	DurationS  int64                 `json:"duration_s"`
	ExitCode   int                   `json:"exit_code"`
	PeakRSSKB  int64                 `json:"peak_rss_kb,omitempty"`
	MaxGPUMiB  int64                 `json:"max_gpu_mem_mib,omitempty"`
	CPUMean    float64               `json:"cpu_mean,omitempty"`
	Timeseries []db.TimeseriesSample `json:"timeseries,omitempty"`
}

func runExportTrainingData(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

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
			RunID:     run.RunID,
			JobID:     run.JobID,
			Host:      run.Host,
			Command:   run.Command,
			Project:   run.Project,
			GPUClass:  run.GPUClass,
			Backend:   run.Backend,
			Tenant:    run.Tenant,
			DurationS: run.DurationS,
			ExitCode:  run.ExitCode,
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

		// Fetch timeseries
		ts, err := db.GetTimeseriesByRun(database, run.RunID)
		if err == nil && len(ts) > 0 {
			rec.Timeseries = ts
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
