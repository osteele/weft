package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var telemetryJSON bool

var jobTelemetryCmd = &cobra.Command{
	Use:   "telemetry <job-id> [job-id...]",
	Short: "Show GPU telemetry summary for a job",
	Long:  `Reads timeseries data for a job and summarises GPU temperature, utilisation, and clock stats. Flags thermal throttling if temperature exceeded 80°C.`,
	Args:  cobra.MinimumNArgs(1),
	RunE:  runTelemetry,
}

var telemetryCmd = &cobra.Command{
	Use:   "telemetry <job-id> [job-id...]",
	Short: "Show GPU telemetry summary for a job",
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
	JobID     int64                 `json:"job_id"`
	TimeMin   int64                 `json:"time_min"`
	TimeMax   int64                 `json:"time_max"`
	DurationS float64               `json:"duration_s"`
	GPU       *db.GPUTelemetryStats `json:"gpu,omitempty"`
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

	var jsonResults []telemetryOutput

	for i, jobID := range jobIDs {
		if !telemetryJSON && len(jobIDs) > 1 && i > 0 {
			fmt.Println("---")
		}

		samples, err := db.GetTimeseries(database, jobID)
		if err != nil {
			return fmt.Errorf("job %d: get timeseries: %w", jobID, err)
		}

		if telemetryJSON {
			out := telemetryOutput{JobID: jobID}
			if len(samples) > 0 {
				out.TimeMin = samples[0].Ts
				out.TimeMax = samples[len(samples)-1].Ts
				out.DurationS = float64(out.TimeMax - out.TimeMin)
				out.GPU = db.ComputeGPUTelemetryStats(samples)
			}
			jsonResults = append(jsonResults, out)
			continue
		}

		if len(samples) == 0 {
			fmt.Printf("Job %d: no telemetry data\n", jobID)
			continue
		}

		stats := db.ComputeGPUTelemetryStats(samples)
		if stats == nil {
			fmt.Printf("Job %d: no GPU telemetry data (%d samples)\n", jobID, len(samples))
			continue
		}

		fmt.Printf("Job %d — GPU Telemetry (%d samples)\n", jobID, stats.SampleCount)

		first := time.Unix(samples[0].Ts, 0)
		last := time.Unix(samples[len(samples)-1].Ts, 0)
		fmt.Printf("  Time range: %s — %s (%s)\n",
			first.Format("15:04:05"),
			last.Format("15:04:05"),
			last.Sub(first).Round(time.Second))
		if stats.TempMax > 0 {
			fmt.Printf("  Temperature: %d–%d°C (mean %.0f°C)\n", stats.TempMin, stats.TempMax, stats.TempMean)
		}
		if stats.UtilMax > 0 {
			fmt.Printf("  Utilisation: %d–%d%% (mean %.0f%%)\n", stats.UtilMin, stats.UtilMax, stats.UtilMean)
		}
		if stats.ClockMax > 0 {
			fmt.Printf("  Clock:       %d–%d MHz (mean %.0f MHz)\n", stats.ClockMin, stats.ClockMax, stats.ClockMean)
		}
		if stats.MemPeakMiB > 0 {
			if stats.MemTotalMiB > 0 {
				fmt.Printf("  GPU Memory:  %d / %d MiB (peak)\n", stats.MemPeakMiB, stats.MemTotalMiB)
			} else {
				fmt.Printf("  GPU Memory:  %d MiB (peak)\n", stats.MemPeakMiB)
			}
		}
		if stats.Throttled {
			fmt.Printf("  ⚠ Thermal throttling likely (temp > %d°C)\n", db.ThermalThrottleThresholdC)
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
