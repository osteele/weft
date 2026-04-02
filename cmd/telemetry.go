package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var telemetryJSON bool

var jobTelemetryCmd = &cobra.Command{
	Use:   "telemetry <job-id> [job-id...]",
	Short: "Show GPU telemetry summary for a job",
	Long:  `Reads telemetry data for a job's latest attempt and summarises GPU temperature, utilisation, clock, and memory stats.`,
	Args:  cobra.MinimumNArgs(1),
	RunE:  runTelemetry,
}

var telemetryCmd = &cobra.Command{
	Use:   "telemetry <job-id> [job-id...]",
	Short: "Show GPU telemetry summary for a job",
	Long:  jobTelemetryCmd.Long,
	Args:  cobra.MinimumNArgs(1),
	RunE:  runTelemetry,
}

func init() {
	jobCmd.AddCommand(jobTelemetryCmd)
	rootCmd.AddCommand(telemetryCmd)
	jobTelemetryCmd.Flags().BoolVar(&telemetryJSON, "json", false, "Emit machine-readable JSON")
	telemetryCmd.Flags().BoolVar(&telemetryJSON, "json", false, "Emit machine-readable JSON")
}

type telemetryOutput struct {
	JobID     int64                   `json:"job_id"`
	AttemptID int64                   `json:"attempt_id,omitempty"`
	TimeMin   int64                   `json:"time_min"`
	TimeMax   int64                   `json:"time_max"`
	DurationS float64                 `json:"duration_s"`
	Samples   int                     `json:"samples"`
	GPU       *db.GPUTelemetryStats   `json:"gpu,omitempty"`
	Summary   *db.JobTelemetrySummary `json:"summary,omitempty"`
}

func runTelemetry(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var jobsToSync []*db.Job
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil {
			continue
		}
		jobsToSync = append(jobsToSync, job)
	}
	if len(jobsToSync) > 0 {
		quickSyncJobs(database, jobsToSync, FastSyncTimeout, FastCloudSyncTimeout)
	}

	var jsonResults []telemetryOutput

	for i, jobID := range jobIDs {
		if !telemetryJSON && len(jobIDs) > 1 && i > 0 {
			fmt.Println("---")
		}

		out, err := collectTelemetryOutput(database, jobID)
		if err != nil {
			return fmt.Errorf("job %d: %w", jobID, err)
		}

		if telemetryJSON {
			jsonResults = append(jsonResults, out)
			continue
		}

		if out.Samples == 0 {
			fmt.Printf("Job %d: no telemetry data\n", jobID)
			continue
		}

		if out.GPU == nil && (out.Summary == nil || len(out.Summary.GPUs) == 0) {
			fmt.Printf("Job %d: no GPU telemetry data (%d samples)\n", jobID, out.Samples)
			continue
		}

		fmt.Printf("Job %d — GPU Telemetry (%d samples)\n", jobID, out.Samples)

		first := time.Unix(out.TimeMin, 0)
		last := time.Unix(out.TimeMax, 0)
		fmt.Printf("  Time range: %s — %s (%s)\n",
			first.Format("15:04:05"),
			last.Format("15:04:05"),
			last.Sub(first).Round(time.Second))
		if out.GPU != nil && out.GPU.TempMax > 0 {
			fmt.Printf("  Temperature: %d–%d°C (mean %.0f°C)\n", out.GPU.TempMin, out.GPU.TempMax, out.GPU.TempMean)
		}
		if out.GPU != nil && out.GPU.UtilMax > 0 {
			fmt.Printf("  Utilisation: %d–%d%% (mean %.0f%%)\n", out.GPU.UtilMin, out.GPU.UtilMax, out.GPU.UtilMean)
		}
		if out.GPU != nil && out.GPU.ClockMax > 0 {
			fmt.Printf("  Clock:       %d–%d MHz (mean %.0f MHz)\n", out.GPU.ClockMin, out.GPU.ClockMax, out.GPU.ClockMean)
		}
		if out.GPU != nil && out.GPU.MemPeakMiB > 0 {
			if out.GPU.MemTotalMiB > 0 {
				fmt.Printf("  GPU Memory:  %d / %d MiB (peak)\n", out.GPU.MemPeakMiB, out.GPU.MemTotalMiB)
			} else {
				fmt.Printf("  GPU Memory:  %d MiB (peak)\n", out.GPU.MemPeakMiB)
			}
		}
		if out.GPU != nil && out.GPU.Throttled {
			fmt.Printf("  ⚠ Thermal throttling likely (temp > %d°C)\n", db.ThermalThrottleThresholdC)
		}
		if out.Summary != nil {
			for _, gpu := range out.Summary.GPUs {
				var parts []string
				label := gpu.GPUIndex
				if gpu.GPUName != "" {
					label = fmt.Sprintf("%s (%s)", gpu.GPUIndex, gpu.GPUName)
				}
				if gpu.GPUMeanUtilPct != nil {
					parts = append(parts, fmt.Sprintf("mean util %.0f%%", *gpu.GPUMeanUtilPct))
				}
				if gpu.GPUMeanMemUtilPct != nil {
					parts = append(parts, fmt.Sprintf("mean mem util %.0f%%", *gpu.GPUMeanMemUtilPct))
				}
				if gpu.GPUPeakMemMiB > 0 {
					parts = append(parts, fmt.Sprintf("peak mem %d MiB", gpu.GPUPeakMemMiB))
				}
				if gpu.GPUActiveSeconds > 0 {
					parts = append(parts, fmt.Sprintf("active %s", (time.Duration(gpu.GPUActiveSeconds*float64(time.Second))).Round(time.Second)))
				}
				if len(parts) > 0 {
					fmt.Printf("  GPU %s: %s\n", label, strings.Join(parts, ", "))
				}
			}
		}
	}

	if telemetryJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if len(jsonResults) == 1 {
			return enc.Encode(jsonResults[0])
		}
		return enc.Encode(jsonResults)
	}
	return nil
}

func collectTelemetryOutput(database *sql.DB, jobID int64) (telemetryOutput, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return telemetryOutput{}, fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return telemetryOutput{}, fmt.Errorf("job not found")
	}

	out := telemetryOutput{JobID: jobID}
	if job.LatestRunID == nil {
		return out, nil
	}
	out.AttemptID = *job.LatestRunID

	legacySamples, err := db.GetTimeseriesByRun(database, *job.LatestRunID)
	if err != nil {
		return telemetryOutput{}, fmt.Errorf("get latest-run timeseries: %w", err)
	}
	richSamples, err := db.GetTelemetryByRun(database, *job.LatestRunID)
	if err != nil {
		return telemetryOutput{}, fmt.Errorf("get latest-run telemetry: %w", err)
	}

	if len(richSamples) > 0 {
		out.TimeMin = richSamples[0].Ts
		out.TimeMax = richSamples[len(richSamples)-1].Ts
		out.Samples = len(richSamples)
	} else if len(legacySamples) > 0 {
		out.TimeMin = legacySamples[0].Ts
		out.TimeMax = legacySamples[len(legacySamples)-1].Ts
		out.Samples = len(legacySamples)
	}
	if out.TimeMin > 0 && out.TimeMax >= out.TimeMin {
		out.DurationS = float64(out.TimeMax - out.TimeMin)
	}

	out.GPU = db.ComputeGPUTelemetryStats(legacySamples)
	if len(richSamples) > 0 {
		var wallDuration float64
		if job.StartTime > 0 && job.EndTime != nil && *job.EndTime > job.StartTime {
			wallDuration = float64(*job.EndTime - job.StartTime)
		}
		out.Summary = db.SummarizeTelemetry(richSamples, wallDuration, splitTelemetryGPUDevices(job))
	}

	return out, nil
}

func splitTelemetryGPUDevices(job *db.Job) []string {
	if job == nil || job.Metadata == nil || job.Metadata.Resource == nil {
		return nil
	}
	devices := strings.TrimSpace(job.Metadata.Resource.GPUDevices)
	if devices == "" {
		return nil
	}
	parts := strings.Split(devices, ",")
	var result []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}
