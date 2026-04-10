package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/r2keys"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

var syncInspectCmd = &cobra.Command{
	Use:   "inspect [dir]",
	Short: "Inspect the local source snapshot",
	Long: `Inspect the local source snapshot using the same exclude rules as
weft source sync and campaign tarballs.

Examples:
  weft sync inspect
  weft sync inspect ~/code/project
  weft sync inspect --json
  weft sync inspect --show-excludes`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
	RunE: runSyncInspect,
}

var (
	syncInspectTopFiles     int
	syncInspectTopDirs      int
	syncInspectJSON         bool
	syncInspectShowExcludes bool
	syncInspectInputs       []string
)

func init() {
	syncCmd.AddCommand(syncInspectCmd)
	syncInspectCmd.Flags().IntVar(&syncInspectTopFiles, "top-files", 10, "Show the N largest included files")
	syncInspectCmd.Flags().IntVar(&syncInspectTopDirs, "top-dirs", 10, "Show the N largest included top-level directories")
	syncInspectCmd.Flags().BoolVar(&syncInspectJSON, "json", false, "Emit machine-readable JSON")
	syncInspectCmd.Flags().BoolVar(&syncInspectShowExcludes, "show-excludes", false, "Print the effective exclude patterns")
	syncInspectCmd.Flags().StringSliceVar(&syncInspectInputs, "input", nil, "Declared inputs to include as overlays (e.g., local:data/conllu/)")
}

func runSyncInspect(cmd *cobra.Command, args []string) error {
	localDir := "."
	if len(args) == 1 {
		localDir = args[0]
	}

	var (
		inspection *srcsync.SnapshotInspection
		err        error
	)
	if len(syncInspectInputs) > 0 {
		inspection, err = srcsync.InspectSnapshotWithInputs(localDir, syncInspectInputs, syncInspectTopFiles, syncInspectTopDirs)
	} else {
		inspection, err = srcsync.InspectSnapshot(localDir, syncInspectTopFiles, syncInspectTopDirs)
	}
	if err != nil {
		return err
	}

	// Enrich with R2 upload status at the cmd layer (best-effort, nil if unconfigured or over limit).
	type output struct {
		*srcsync.SnapshotInspection
		R2Key      string `json:"r2_key,omitempty"`
		R2Uploaded *bool  `json:"r2_uploaded,omitempty"`
	}
	report := output{SnapshotInspection: inspection}
	if inspection.TarballHash != "" {
		if cfg, err := config.Load(); err == nil {
			if r2Client, err := buildR2Client(cfg); err == nil && r2Client != nil {
				report.R2Key = r2keys.SourceTarball(inspection.TarballHash)
				if uploaded, err := r2Client.ObjectExists(context.Background(), report.R2Key); err == nil {
					report.R2Uploaded = &uploaded
				}
			}
		}
	}

	if syncInspectJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Source snapshot: %s\n", inspection.LocalDir)
	fmt.Fprintf(out, "Included files: %d\n", inspection.FileCount)
	fmt.Fprintf(out, "Included directories: %d\n", inspection.DirectoryCount)
	fmt.Fprintf(out, "Included size: %s\n", terminal.FormatBytesIEC(inspection.TotalBytes))
	if inspection.CompressedBytes != nil {
		fmt.Fprintf(out, "Compressed tarball: %s\n", terminal.FormatBytesIEC(*inspection.CompressedBytes))
	} else {
		fmt.Fprintf(out, "Compressed tarball: skipped (included size exceeds %s limit)\n", terminal.FormatBytesIEC(inspection.LimitBytes))
	}
	switch {
	case inspection.OverLimit:
		fmt.Fprintf(out, "R2 upload: skipped (over size limit)\n")
	case report.R2Uploaded == nil:
		fmt.Fprintf(out, "R2 upload: not configured\n")
	case *report.R2Uploaded:
		fmt.Fprintf(out, "R2 upload: yes\n")
	default:
		fmt.Fprintf(out, "R2 upload: no\n")
	}
	if inspection.OverLimit {
		fmt.Fprintf(out, "Snapshot limit: exceeded by %s\n", terminal.FormatBytesIEC(report.TotalBytes-report.LimitBytes))
	} else {
		fmt.Fprintf(out, "Snapshot limit: within %s\n", terminal.FormatBytesIEC(report.LimitBytes))
	}

	if len(report.Overlays) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Declared local inputs (overlaid on snapshot):")
		for _, overlay := range report.Overlays {
			fmt.Fprintf(out, "  %s\n", overlay)
		}
	}

	printSnapshotItems(out, "Largest included files", report.LargestFiles)
	printSnapshotItems(out, "Largest included top-level directories", report.LargestTopLevel)

	if syncInspectShowExcludes {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Exclude patterns:")
		for _, pattern := range report.Excludes {
			fmt.Fprintf(out, "  %s\n", pattern)
		}
	}

	return nil
}

func printSnapshotItems(out io.Writer, heading string, items []srcsync.SnapshotItem) {
	fmt.Fprintf(out, "\n%s:\n", heading)
	if len(items) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}
	sizeWidth := 0
	approxWidth := 0
	for _, item := range items {
		sizeWidth = max(sizeWidth, len(terminal.FormatBytesIEC(item.Bytes)))
		approxWidth = max(approxWidth, len("~"+terminal.FormatBytesIEC(item.ApproxCompressedBytes)))
	}
	for _, item := range items {
		fmt.Fprintf(out, "  %*s  %*s  %s\n",
			sizeWidth, terminal.FormatBytesIEC(item.Bytes),
			approxWidth, "~"+terminal.FormatBytesIEC(item.ApproxCompressedBytes),
			item.Path,
		)
	}
}
