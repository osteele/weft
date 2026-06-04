package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/timeseriescache"
	"github.com/spf13/cobra"
)

var (
	jobTimeseriesRaw     bool
	jobTimeseriesRunID   int64
	jobTimeseriesSummary bool
)

var jobTimeseriesCmd = &cobra.Command{
	Use:   "timeseries <job-id>",
	Short: "Print per-sample telemetry for a job",
	Long: `Streams the agent's per-sample timeseries (CPU, RSS, GPU, disk free/total,
GPU temp, etc.) for a job's most recent run.

Raw telemetry is retained in R2 and cached locally under ~/.cache/weft; SQLite
keeps compact summaries for ordinary status and estimation queries. Use --raw
for the JSONL file as the agent wrote it. Use --summary for high-water marks only.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runJobTimeseries,
}

func runJobTimeseries(_ *cobra.Command, args []string) error {
	jobID, err := ids.ParseJobID(args[0])
	if err != nil {
		return usageErrorf("invalid job ID %q", args[0])
	}

	database, err := db.OpenForReading()
	if err != nil {
		return err
	}
	defer database.Close()

	runID := jobTimeseriesRunID
	if runID == 0 {
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil {
			return fmt.Errorf("look up job: %w", err)
		}
		if job.LatestRunID == nil || *job.LatestRunID == 0 {
			return fmt.Errorf("job %s has no recorded run id; pass --run", ids.FormatJobID(jobID))
		}
		runID = *job.LatestRunID
	}

	if jobTimeseriesRaw {
		data, _, err := loadRawTimeseries(database, jobID, runID)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			fmt.Printf("No raw timeseries found for %s run=%d\n", ids.FormatJobID(jobID), runID)
			return nil
		}
		fmt.Print(string(data))
		return nil
	}

	summary, err := loadTimeseriesSummary(database, jobID, runID)
	if err != nil {
		return err
	}
	if summary == nil || summary.SampleCount == 0 {
		fmt.Printf("No timeseries found for %s run=%d\n", ids.FormatJobID(jobID), runID)
		return nil
	}

	printTimeseriesSummary(summary)
	if !jobTimeseriesSummary {
		fmt.Println()
		fmt.Println("(use --raw to stream the full JSONL, --run <id> to pick a specific attempt)")
	}
	return nil
}

func loadTimeseriesSummary(database *sql.DB, jobID, runID int64) (*db.TimeseriesSummary, error) {
	summary, err := db.GetTimeseriesSummaryByRun(database, runID)
	if err != nil {
		return nil, err
	}
	if summary != nil {
		return summary, nil
	}
	if samples, err := db.GetTimeseriesByRun(database, runID); err != nil {
		return nil, err
	} else if len(samples) > 0 {
		return db.SummarizeTimeseries(jobID, runID, samples), nil
	}
	data, _, err := loadRawTimeseries(database, jobID, runID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return summarizeTimeseriesJSONL(data, jobID, runID), nil
}

func printTimeseriesSummary(summary *db.TimeseriesSummary) {
	fmt.Printf("Timeseries for %s run=%d (%d samples)\n",
		ids.FormatJobID(summary.JobID), summary.AttemptID, summary.SampleCount)
	fmt.Printf("  span:        %s → %s (%s)\n",
		time.Unix(summary.TSMin, 0).Format(time.RFC3339),
		time.Unix(summary.TSMax, 0).Format(time.RFC3339),
		(time.Duration(summary.TSMax-summary.TSMin) * time.Second).String())
	if summary.LastDiskTotalBytes > 0 {
		fmt.Printf("  disk total:  %s (last sample)\n", formatBytes(summary.LastDiskTotalBytes))
	}
	if summary.PeakDiskUsedBytes > 0 {
		pct := 0.0
		if summary.LastDiskTotalBytes > 0 {
			pct = float64(summary.PeakDiskUsedBytes) / float64(summary.LastDiskTotalBytes) * 100
		}
		fmt.Printf("  disk peak:   %s (%.1f%%)\n", formatBytes(summary.PeakDiskUsedBytes), pct)
	}
	if summary.PeakRSSKB > 0 {
		fmt.Printf("  RSS peak:    %s\n", formatBytes(summary.PeakRSSKB*1024))
	}
	if summary.PeakGPUMemMiB > 0 {
		fmt.Printf("  GPU mem:     %d MiB peak\n", summary.PeakGPUMemMiB)
	}
	if summary.PeakGPUUtilPct > 0 {
		fmt.Printf("  GPU util:    %d%% peak", summary.PeakGPUUtilPct)
		if summary.MeanGPUUtilPct > 0 {
			fmt.Printf(", %.1f%% mean", summary.MeanGPUUtilPct)
		}
		fmt.Println()
	}
	if summary.PeakGPUTempC > 0 {
		fmt.Printf("  GPU temp:    %d°C peak", summary.PeakGPUTempC)
		if summary.MeanGPUTempC > 0 {
			fmt.Printf(", %.1f°C mean", summary.MeanGPUTempC)
		}
		fmt.Println()
	}
}

func loadRawTimeseries(database *sql.DB, jobID, runID int64) ([]byte, string, error) {
	if obj, err := db.GetTimeseriesRawObject(database, runID, db.TimeseriesRawKind); err != nil {
		return nil, "", err
	} else if obj != nil && obj.R2Key != "" {
		if data, ok := timeseriescache.Read(jobID, runID, obj.ETag); ok {
			return data, "cache", nil
		}
		data, err := fetchR2TimeseriesObject(obj.R2Key)
		if err != nil {
			return nil, "", err
		}
		_ = timeseriescache.Write(jobID, runID, obj.ETag, data)
		return data, "r2", nil
	}

	if data, key, err := loadLegacyRawTimeseriesFromR2(jobID, runID); err == nil && len(data) > 0 {
		return data, key, nil
	}

	samples, err := db.GetTimeseriesByRun(database, runID)
	if err != nil {
		return nil, "", err
	}
	if len(samples) == 0 {
		return nil, "", os.ErrNotExist
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, sample := range samples {
		if err := enc.Encode(sample); err != nil {
			return nil, "", err
		}
	}
	return []byte(b.String()), "db", nil
}

func fetchR2TimeseriesObject(key string) ([]byte, error) {
	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := r2Client.GetObject(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", key, err)
	}
	return data, nil
}

func loadLegacyRawTimeseriesFromR2(jobID, runID int64) ([]byte, string, error) {
	keys := []string{
		r2keys.JobAttemptRawTimeseries(jobID, runID),
		fmt.Sprintf("jobs/%d/runs/%d/results/%d.timeseries.jsonl", jobID, runID, jobID),
	}
	for _, key := range keys {
		data, err := fetchR2TimeseriesObject(key)
		if err == nil && len(data) > 0 {
			return data, key, nil
		}
	}
	return nil, "", os.ErrNotExist
}

func summarizeTimeseriesJSONL(data []byte, jobID, runID int64) *db.TimeseriesSummary {
	samples := db.ParseTimeseriesJSONL(string(data), 0, "")
	return db.SummarizeTimeseries(jobID, runID, samples)
}
