package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/spf13/cobra"
)

var instanceDiskReportCmd = &cobra.Command{
	Use:   "disk-report <instance-id>",
	Short: "Print the agent's disk-failure report (uploaded to R2 on disk_full)",
	Long: `Fetches instance/<id>/disk-failure.json from R2 and prints it.

The agent uploads this report when it hits ENOSPC: container df, top
directory sizes (HF cache, uv cache, workspace), and a per-asset HF cache
inventory. This is the most direct evidence of why a container ran out
of disk and what was on it at the moment of failure.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runInstanceDiskReport,
}

type diskReportEnvelope struct {
	TimestampUnix     int64             `json:"timestamp_unix"`
	Phase             string            `json:"phase"`
	JobID             int64             `json:"job_id"`
	FilesystemPath    string            `json:"filesystem_path"`
	BallastPath       string            `json:"ballast_path"`
	BallastSizeBytes  int64             `json:"ballast_size_bytes"`
	FreeBytesBefore   int64             `json:"free_bytes_before"`
	FreeBytesAfterDel int64             `json:"free_bytes_after_delete"`
	DfH               string            `json:"df_h"`
	DirectoryUsage    map[string]string `json:"directory_usage"`
	HFCacheModels     []hfCacheLine     `json:"hf_cache_models"`
}

type hfCacheLine struct {
	AssetID   string `json:"asset_id"`
	SizeBytes int64  `json:"size_bytes"`
}

func runInstanceDiskReport(_ *cobra.Command, args []string) error {
	instanceID, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return usageErrorf("invalid instance ID %q", args[0])
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	data, err := r2Client.GetObject(ctx, r2keys.InstanceDiskFailure(instanceID))
	if err != nil {
		return fmt.Errorf("fetch disk-failure report: %w", err)
	}
	if len(data) == 0 {
		fmt.Printf("No disk-failure report on R2 for %s. The agent only uploads this on ENOSPC.\n",
			ids.FormatInstanceID(instanceID))
		return nil
	}

	var rep diskReportEnvelope
	if err := json.Unmarshal(data, &rep); err != nil {
		return fmt.Errorf("parse disk-failure report: %w", err)
	}

	fmt.Printf("Disk-failure report for %s\n", ids.FormatInstanceID(instanceID))
	if rep.TimestampUnix > 0 {
		fmt.Printf("  reported_at:    %s\n", time.Unix(rep.TimestampUnix, 0).Format(time.RFC3339))
	}
	if rep.Phase != "" {
		fmt.Printf("  phase:          %s\n", rep.Phase)
	}
	if rep.JobID > 0 {
		fmt.Printf("  job:            %s\n", ids.FormatJobID(rep.JobID))
	}
	if rep.FilesystemPath != "" {
		fmt.Printf("  filesystem:     %s\n", rep.FilesystemPath)
	}
	fmt.Printf("  free_before:    %s\n", formatBytesIEC(rep.FreeBytesBefore))
	fmt.Printf("  free_after_del: %s (after deleting %s ballast)\n",
		formatBytesIEC(rep.FreeBytesAfterDel), formatBytesIEC(rep.BallastSizeBytes))

	// Compare reported container disk against what was launched-with on the
	// launch row — surfaces silent provider-side disk caps.
	if database, err := db.OpenForReading(); err == nil {
		defer database.Close()
		if launch, err := db.GetLaunch(database, instanceID); err == nil && launch != nil {
			fmt.Printf("  recorded_disk:  %d GB (from launches.disk_gb — what weft requested)\n", launch.DiskGB)
		}
	}

	if rep.DfH != "" {
		fmt.Println("\ndf -h:")
		fmt.Println(rep.DfH)
	}

	if len(rep.DirectoryUsage) > 0 {
		fmt.Println("\nTop directories:")
		keys := make([]string, 0, len(rep.DirectoryUsage))
		for k := range rep.DirectoryUsage {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-12s %s\n", k+":", rep.DirectoryUsage[k])
		}
	}

	if len(rep.HFCacheModels) > 0 {
		fmt.Println("\nHF cache contents (asset → on-disk size):")
		sort.Slice(rep.HFCacheModels, func(i, j int) bool {
			return rep.HFCacheModels[i].SizeBytes > rep.HFCacheModels[j].SizeBytes
		})
		var total int64
		for _, m := range rep.HFCacheModels {
			fmt.Printf("  %10s  %s\n", formatBytesIEC(m.SizeBytes), m.AssetID)
			total += m.SizeBytes
		}
		fmt.Printf("  %10s  (total)\n", formatBytesIEC(total))
	}
	return nil
}

func formatBytesIEC(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
