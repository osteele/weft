package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
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
	Job       string                  `json:"job"`
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

	database, err := db.OpenForReading()
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
			return fmt.Errorf("job %s: %w", ids.FormatJobID(jobID), err)
		}

		if telemetryJSON {
			jsonResults = append(jsonResults, out)
			continue
		}

		if out.Samples == 0 {
			fmt.Printf("Job %s: no telemetry data\n", ids.FormatJobID(jobID))
			continue
		}

		if out.GPU == nil && (out.Summary == nil || len(out.Summary.GPUs) == 0) {
			fmt.Printf("Job %s: no GPU telemetry data (%d samples)\n", ids.FormatJobID(jobID), out.Samples)
			continue
		}

		fmt.Printf("Job %s — GPU Telemetry (%d samples)\n", ids.FormatJobID(jobID), out.Samples)

		first := time.Unix(out.TimeMin, 0)
		last := time.Unix(out.TimeMax, 0)
		fmt.Printf("  Time range: %s — %s (%s)\n",
			first.Format("15:04:05"),
			last.Format("15:04:05"),
			last.Sub(first).Round(time.Second))
		if g := out.GPU; g != nil {
			// A range needs both ends; report whichever the source could
			// supply rather than printing a fabricated bound beside a real one.
			if g.TempMax != nil {
				fmt.Printf("  Temperature: %s (mean %s)\n", formatRangeInt(g.TempMin, g.TempMax, "°C"), formatMean(g.TempMean, "°C"))
			}
			if g.UtilMax != nil {
				fmt.Printf("  Utilisation: %s (mean %s)\n", formatRangeInt(g.UtilMin, g.UtilMax, "%"), formatMean(g.UtilMean, "%"))
			}
			if g.ClockMax != nil {
				fmt.Printf("  Clock:       %s (mean %s)\n", formatRangeInt(g.ClockMin, g.ClockMax, " MHz"), formatMean(g.ClockMean, " MHz"))
			}
			if g.MemPeakMiB != nil {
				if g.MemTotalMiB != nil {
					fmt.Printf("  GPU Memory:  %d / %d MiB (peak)\n", *g.MemPeakMiB, *g.MemTotalMiB)
				} else {
					fmt.Printf("  GPU Memory:  %d MiB (peak)\n", *g.MemPeakMiB)
				}
			}
			if g.Throttled != nil && *g.Throttled {
				fmt.Printf("  ⚠ Thermal throttling likely (temp > %d°C)\n", db.ThermalThrottleThresholdC)
			}
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

// runTelemetrySources holds every telemetry source recorded for one attempt.
// No single one is complete, and `weft telemetry` and `weft job info` must
// summarise an attempt identically, so both read the set through here rather
// than each choosing its own subset.
type runTelemetrySources struct {
	Rollup        *db.TimeseriesSummary
	LegacySamples []db.TimeseriesSample
	RichSamples   []db.TelemetrySample
}

func loadRunTelemetry(database *sql.DB, runID int64) (runTelemetrySources, error) {
	var sources runTelemetrySources
	var err error
	sources.Rollup, err = db.GetTimeseriesSummaryByRun(database, runID)
	if err != nil {
		return runTelemetrySources{}, fmt.Errorf("get latest-run timeseries summary: %w", err)
	}
	if sources.Rollup == nil {
		sources.LegacySamples, err = db.GetTimeseriesByRun(database, runID)
		if err != nil {
			return runTelemetrySources{}, fmt.Errorf("get latest-run timeseries: %w", err)
		}
	}
	sources.RichSamples, err = db.GetTelemetryByRun(database, runID)
	if err != nil {
		return runTelemetrySources{}, fmt.Errorf("get latest-run telemetry: %w", err)
	}
	return sources, nil
}

// gpuStats takes each field from the first source that reports it, so no
// single source's blind spot becomes the attempt's: the rollup carries neither
// clocks nor minima, and the per-device rows carry no temperature.
//
// Order matters. The timeseries sources come first because their utilisation
// and memory are aggregated across the host's GPUs, which is what these fields
// mean to existing consumers; the per-device rows report one card at a time,
// so promoting them would silently redefine mem_peak_mib. The per-device
// source goes last and fills only what the others cannot report at all — in
// practice the GPU clocks.
func (s runTelemetrySources) gpuStats() *db.GPUTelemetryStats {
	return db.MergeGPUTelemetryStats(
		db.ComputeGPUTelemetryStats(s.LegacySamples),
		db.GPUTelemetryStatsFromTimeseriesSummary(s.Rollup),
		db.ComputeGPUTelemetryStatsFromGPUSamples(s.RichSamples),
	)
}

func collectTelemetryOutput(database *sql.DB, jobID int64) (telemetryOutput, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return telemetryOutput{}, fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return telemetryOutput{}, fmt.Errorf("job not found")
	}

	out := telemetryOutput{JobID: jobID, Job: ids.FormatJobID(jobID)}
	if job.LatestRunID == nil {
		return out, nil
	}
	out.AttemptID = *job.LatestRunID

	sources, err := loadRunTelemetry(database, *job.LatestRunID)
	if err != nil {
		return telemetryOutput{}, err
	}
	legacySummary, legacySamples, richSamples := sources.Rollup, sources.LegacySamples, sources.RichSamples

	if len(richSamples) > 0 {
		out.TimeMin = richSamples[0].Ts
		out.TimeMax = richSamples[len(richSamples)-1].Ts
		out.Samples = len(richSamples)
	} else if legacySummary != nil && legacySummary.SampleCount > 0 {
		out.TimeMin = legacySummary.TSMin
		out.TimeMax = legacySummary.TSMax
		out.Samples = legacySummary.SampleCount
	} else if len(legacySamples) > 0 {
		out.TimeMin = legacySamples[0].Ts
		out.TimeMax = legacySamples[len(legacySamples)-1].Ts
		out.Samples = len(legacySamples)
	}
	if out.TimeMin > 0 && out.TimeMax >= out.TimeMin {
		out.DurationS = float64(out.TimeMax - out.TimeMin)
	}

	out.GPU = sources.gpuStats()
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

// formatRangeInt renders a measured range, or just the upper bound when the
// source could not report a minimum. Printing a nil minimum as 0 would invent
// a reading.
func formatRangeInt(lo, hi *int, unit string) string {
	if hi == nil {
		return "n/a"
	}
	if lo == nil {
		return fmt.Sprintf("%d%s (peak)", *hi, unit)
	}
	return fmt.Sprintf("%d–%d%s", *lo, *hi, unit)
}

func formatMean(v *float64, unit string) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%s", *v, unit)
}
