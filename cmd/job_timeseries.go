package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/runner"
	"github.com/spf13/cobra"
)

var (
	jobTimeseriesRaw     bool
	jobTimeseriesRunID   int64
	jobTimeseriesSummary bool
)

var jobTimeseriesCmd = &cobra.Command{
	Use:   "timeseries <job-id>",
	Short: "Print per-sample telemetry for a job from R2",
	Long: `Streams the agent's per-sample timeseries (CPU, RSS, GPU, disk free/total,
GPU temp, etc.) for a job's most recent run from R2.

The local DB does not store this data — telemetry lives in R2 to keep the
DB small. Use --raw for the JSONL file as the agent wrote it. Use
--summary for high-water marks only.`,
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

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	key := fmt.Sprintf("jobs/%d/runs/%d/results/%d.timeseries.jsonl", jobID, runID, jobID)
	data, err := r2Client.GetObject(ctx, key)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", key, err)
	}
	if len(data) == 0 {
		fmt.Printf("No timeseries on R2 at %s\n", key)
		return nil
	}

	if jobTimeseriesRaw {
		fmt.Print(string(data))
		return nil
	}

	var (
		first, last   runner.TimeseriesSample
		nSamples      int
		peakDiskUsed  int64
		peakRSSKB     int64
		peakGPUMemMiB int
		peakGPUUtil   int
		peakGPUTempC  int
		seenAny       bool
	)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s runner.TimeseriesSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue
		}
		if !seenAny {
			first = s
			seenAny = true
		}
		last = s
		nSamples++
		if s.DiskTotalBytes > 0 && s.DiskFreeBytes >= 0 && s.DiskTotalBytes >= s.DiskFreeBytes {
			used := s.DiskTotalBytes - s.DiskFreeBytes
			if used > peakDiskUsed {
				peakDiskUsed = used
			}
		}
		if s.RSSKB > peakRSSKB {
			peakRSSKB = s.RSSKB
		}
		if s.GPUMemUsed > peakGPUMemMiB {
			peakGPUMemMiB = s.GPUMemUsed
		}
		if s.GPUUtilPct > peakGPUUtil {
			peakGPUUtil = s.GPUUtilPct
		}
		if s.GPUTempC > peakGPUTempC {
			peakGPUTempC = s.GPUTempC
		}
	}
	if nSamples == 0 {
		fmt.Println("(empty timeseries)")
		return nil
	}

	fmt.Printf("Timeseries for %s run=%d (%d samples)\n",
		ids.FormatJobID(jobID), runID, nSamples)
	fmt.Printf("  span:        %s → %s (%s)\n",
		time.Unix(first.Ts, 0).Format(time.RFC3339),
		time.Unix(last.Ts, 0).Format(time.RFC3339),
		(time.Duration(last.Ts-first.Ts) * time.Second).String())
	if last.DiskTotalBytes > 0 {
		fmt.Printf("  disk total:  %s (last sample)\n", formatBytes(last.DiskTotalBytes))
	}
	if peakDiskUsed > 0 {
		pct := 0.0
		if last.DiskTotalBytes > 0 {
			pct = float64(peakDiskUsed) / float64(last.DiskTotalBytes) * 100
		}
		fmt.Printf("  disk peak:   %s (%.1f%%)\n", formatBytes(peakDiskUsed), pct)
	}
	if peakRSSKB > 0 {
		fmt.Printf("  RSS peak:    %s\n", formatBytes(peakRSSKB*1024))
	}
	if peakGPUMemMiB > 0 {
		fmt.Printf("  GPU mem:     %d MiB peak\n", peakGPUMemMiB)
	}
	if peakGPUUtil > 0 {
		fmt.Printf("  GPU util:    %d%% peak\n", peakGPUUtil)
	}
	if peakGPUTempC > 0 {
		fmt.Printf("  GPU temp:    %d°C peak\n", peakGPUTempC)
	}
	if jobTimeseriesSummary {
		return nil
	}
	fmt.Println()
	fmt.Println("(use --raw to stream the full JSONL, --run <id> to pick a specific attempt)")
	return nil
}
